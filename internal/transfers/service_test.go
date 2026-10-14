package transfers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela-mcp/internal/app"
	"go.xchunk.org/vela/api"
)

type fakeFile struct {
	object api.Object
	data   []byte
}

type gateway struct {
	app.Gateway
	mu          sync.Mutex
	files       map[string]fakeFile
	uploads     int
	lastOptions app.UploadOptions
	uploadError error
	corruptRead bool
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (g *gateway) Stat(_ context.Context, p string) (*api.Object, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.files[p]
	if !ok {
		return nil, &api.Error{Code: api.CodeNotFound, Message: "not found"}
	}
	o := f.object
	return &o, nil
}

func (g *gateway) Upload(_ context.Context, p string, reader io.Reader, opts app.UploadOptions) (*api.Object, bool, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, false, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.uploads++
	g.lastOptions = opts
	if g.uploadError != nil {
		return nil, false, g.uploadError
	}
	if _, exists := g.files[p]; exists || !opts.CreateOnly || int64(len(data)) != opts.Size || digest(data) != opts.SHA256 {
		return nil, false, &api.Error{Code: api.CodeAlreadyExists, Message: "already exists"}
	}
	o := api.Object{ID: "01arz3ndektsv4rrffq69g5fav", Path: p, Name: filepath.Base(p), Size: opts.Size, SHA256: opts.SHA256, Revision: 1, Visibility: opts.Visibility, ContentType: opts.ContentType, Disposition: opts.Disposition, Tags: opts.Tags, ExpiresAt: opts.ExpiresAt}
	if opts.Title != nil {
		o.Title = *opts.Title
	}
	if opts.Description != nil {
		o.Description = *opts.Description
	}
	g.files[p] = fakeFile{object: o, data: data}
	return &o, true, nil
}

func (g *gateway) Download(_ context.Context, p string) (io.ReadCloser, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.files[p]
	if !ok {
		return nil, &api.Error{Code: api.CodeNotFound, Message: "not found"}
	}
	b := append([]byte(nil), f.data...)
	if g.corruptRead && len(b) > 0 {
		b[0] ^= 0xff
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

type factory struct{ g *gateway }

func (f factory) Open(context.Context, access.Credential) (app.Gateway, error) { return f.g, nil }

type fixture struct {
	s     *Service
	g     *gateway
	app   *app.Service
	mu    sync.Mutex
	users map[string]access.Identity
	owner access.Identity
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	g := &gateway{files: map[string]fakeFile{}}
	owner := access.Identity{Subject: "grant-owner", Scope: api.ScopeRW, Prefix: "team/", Credential: access.NewCredential("fake-upstream-secret")}
	f := &fixture{g: g, owner: owner, users: map[string]access.Identity{owner.Subject: owner}}
	f.app = &app.Service{Resolver: access.ContextResolver{}, Gateways: factory{g}, Limits: app.Limits{MaxUploadBytes: 1024, MaxDownloadBytes: 1024}}
	var err error
	f.s, err = Open(context.Background(), Options{BaseURL: "https://mcp.example.test", DatabasePath: filepath.Join(t.TempDir(), "transfers.db"), Service: f.app, LookupIdentity: f.lookup, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.s.Close() })
	return f
}

func (f *fixture) lookup(_ context.Context, subject string) (access.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.users[subject]
	if !ok {
		return access.Identity{}, access.ErrForbidden
	}
	return i, nil
}

func (f *fixture) ctx() context.Context { return access.WithIdentity(context.Background(), f.owner) }

func (f *fixture) prepare(t *testing.T, p string, data []byte) *UploadGrant {
	t.Helper()
	u, err := f.s.PrepareUpload(f.ctx(), UploadRequest{Path: p, Size: int64(len(data)), SHA256: digest(data)})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func request(s *Service, method, url string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, url, bytes.NewReader(body))
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestUploadBinaryMetadataStatusAndHashedCapability(t *testing.T) {
	f := newFixture(t)
	data := []byte{0, 255, 10, 0, 37, 12, 200}
	title, description := "Report", "Raw bytes"
	u, err := f.s.PrepareUpload(f.ctx(), UploadRequest{Path: "/team/report.bin", Size: int64(len(data)), SHA256: strings.ToUpper(digest(data)), Metadata: app.Metadata{Title: &title, Description: &description, Tags: []string{"TAG"}, ContentType: "application/octet-stream", Disposition: api.Attachment, ExpiresAt: "1d"}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.s.repo.get(context.Background(), u.TransferID)
	if err != nil {
		t.Fatal(err)
	}
	capability := strings.TrimPrefix(u.Headers["Authorization"], "Bearer ")
	if v.Hash == capability || strings.Contains(string(v.Binding), "fake-upstream-secret") || strings.Contains(string(v.Binding), capability) {
		t.Fatal("capability or credential persisted in plaintext")
	}
	w := request(f.s, "PUT", u.UploadURL, data, u.Headers)
	if w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	status, err := f.s.Status(f.ctx(), u.TransferID)
	if err != nil || status.State != "succeeded" || status.Upload == nil || status.Upload.Object.Path != "team/report.bin" {
		t.Fatalf("missing receipt: %+v %v", status, err)
	}
	f.g.mu.Lock()
	o := f.g.lastOptions
	f.g.mu.Unlock()
	if !o.CreateOnly || o.IfRevision != 0 || o.Visibility != api.Private || o.Title == nil || *o.Title != title || o.Description == nil || *o.Description != description || o.Tags[0] != "tag" || o.ExpiresAt == nil || o.ContentType != "application/octet-stream" || o.Disposition != api.Attachment {
		t.Fatal("bound metadata or create-only defaults were lost")
	}
	if w := request(f.s, "PUT", u.UploadURL, data, u.Headers); w.Code != http.StatusConflict {
		t.Fatalf("single-use capability replay: %d", w.Code)
	}
	entries, err := os.ReadDir(f.s.spool)
	if err != nil || len(entries) != 0 {
		t.Fatal("spool not cleaned")
	}
}

func TestUploadExactSizeHashAndNoOverwrite(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
	}{
		{"short", []byte{1}}, {"long", []byte{1, 2, 3}}, {"wrong-checksum", []byte{2, 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			u := f.prepare(t, "team/x.bin", []byte{1, 2})
			w := request(f.s, "PUT", u.UploadURL, test.body, u.Headers)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("body mismatch accepted: %d %s", w.Code, w.Body.String())
			}
			if f.g.uploads != 0 {
				t.Fatal("invalid bytes reached mutation gateway")
			}
			status, _ := f.s.Status(f.ctx(), u.TransferID)
			if status.State != "failed" {
				t.Fatalf("status: %+v", status)
			}
		})
	}
	f := newFixture(t)
	u := f.prepare(t, "team/x.bin", []byte{1, 2})
	f.g.files["team/x.bin"] = fakeFile{object: api.Object{Path: "team/x.bin", SHA256: digest([]byte{9})}, data: []byte{9}}
	w := request(f.s, "PUT", u.UploadURL, []byte{1, 2}, u.Headers)
	if w.Code != http.StatusConflict || !bytes.Equal(f.g.files["team/x.bin"].data, []byte{9}) {
		t.Fatalf("create-only overwrite protection: %d", w.Code)
	}
}

func TestAuthorityIsolationExpiryAndRevocation(t *testing.T) {
	f := newFixture(t)
	data := []byte{1, 2}
	u := f.prepare(t, "team/x.bin", data)
	other := f.owner
	other.Subject = "other-grant"
	if _, err := f.s.Status(access.WithIdentity(context.Background(), other), u.TransferID); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("other grant accessed status")
	}
	if _, err := f.s.PrepareUpload(f.ctx(), UploadRequest{Path: "outside/x", Size: 2, SHA256: digest(data)}); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("upload escaped prefix")
	}
	ro := f.owner
	ro.Scope = api.ScopeRO
	if _, err := f.s.PrepareUpload(access.WithIdentity(context.Background(), ro), UploadRequest{Path: "team/x", Size: 2, SHA256: digest(data)}); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("read-only identity prepared mutation")
	}
	if w := request(f.s, "PUT", u.UploadURL, data, map[string]string{"Authorization": "Bearer " + strings.Repeat("x", 43)}); w.Code != http.StatusUnauthorized {
		t.Fatal("wrong capability accepted")
	}
	f.mu.Lock()
	delete(f.users, f.owner.Subject)
	f.mu.Unlock()
	if w := request(f.s, "PUT", u.UploadURL, data, u.Headers); w.Code != http.StatusUnauthorized || f.g.uploads != 0 {
		t.Fatal("revoked authorization mutated Vela")
	}
	f.s.now = func() time.Time { return u.ExpiresAt.Add(time.Second) }
	if w := request(f.s, "PUT", u.UploadURL, data, u.Headers); w.Code != http.StatusGone {
		t.Fatal("expired capability accepted")
	}
}

func TestConcurrentUploadClaim(t *testing.T) {
	f := newFixture(t)
	data := []byte{1, 2}
	u := f.prepare(t, "team/x.bin", data)
	var wg sync.WaitGroup
	codes := make(chan int, 16)
	for range 16 {
		wg.Go(func() { codes <- request(f.s, "PUT", u.UploadURL, data, u.Headers).Code })
	}
	wg.Wait()
	close(codes)
	created := 0
	for code := range codes {
		if code == http.StatusCreated {
			created++
		} else if code != http.StatusConflict && code != http.StatusServiceUnavailable {
			t.Fatalf("unexpected concurrent response: %d", code)
		}
	}
	if created != 1 || f.g.uploads != 1 {
		t.Fatalf("upload was claimed repeatedly: %d successes, %d upstream writes", created, f.g.uploads)
	}
}

func TestDownloadOriginalBinaryRangesVersionBindingAndRevocation(t *testing.T) {
	f := newFixture(t)
	data := []byte{0, 255, 10, 0, 37, 12, 200}
	u := f.prepare(t, "team/report.pdf", data)
	if w := request(f.s, "PUT", u.UploadURL, data, u.Headers); w.Code != http.StatusCreated {
		t.Fatal(w.Body.String())
	}
	ro := f.owner
	ro.Scope = api.ScopeRO
	d, err := f.s.PrepareDownload(access.WithIdentity(context.Background(), ro), "team/report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	w := request(f.s, "GET", d.DownloadURL, nil, nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") || w.Header().Get("ETag") != `"sha256:`+digest(data)+`"` || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("binary download: %d %q %s", w.Code, w.Body.Bytes(), w.Header())
	}
	w = request(f.s, "GET", d.DownloadURL, nil, map[string]string{"Range": "bytes=1-3"})
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), data[1:4]) || w.Header().Get("Content-Range") != "bytes 1-3/7" {
		t.Fatalf("range: %d %q %s", w.Code, w.Body.Bytes(), w.Header())
	}
	w = request(f.s, "HEAD", d.DownloadURL, nil, nil)
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "7" {
		t.Fatalf("HEAD: %d %s", w.Code, w.Header())
	}
	f.g.corruptRead = true
	if w = request(f.s, "GET", d.DownloadURL, nil, nil); w.Code != http.StatusBadGateway || bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("unchecked corruption was served")
	}
	f.g.corruptRead = false
	f.g.mu.Lock()
	file := f.g.files["team/report.pdf"]
	file.object.Revision++
	f.g.files["team/report.pdf"] = file
	f.g.mu.Unlock()
	if w = request(f.s, "GET", d.DownloadURL, nil, nil); w.Code != http.StatusConflict {
		t.Fatal("replacement escaped content-version binding")
	}
	f.mu.Lock()
	delete(f.users, f.owner.Subject)
	f.mu.Unlock()
	if w = request(f.s, "GET", d.DownloadURL, nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatal("revoked download still worked")
	}
	if f.g.files["team/report.pdf"].object.Visibility != api.Private {
		t.Fatal("download changed visibility")
	}
}

func TestPersistentRecoveryAndUnknownUpstreamOutcome(t *testing.T) {
	f := newFixture(t)
	data := []byte{1, 2}
	u := f.prepare(t, "team/x.bin", data)
	f.g.uploadError = app.ErrUnavailable
	if w := request(f.s, "PUT", u.UploadURL, data, u.Headers); w.Code != http.StatusBadGateway {
		t.Fatal("upstream error missing")
	}
	status, err := f.s.Status(f.ctx(), u.TransferID)
	if err != nil || status.State != "uncertain" {
		t.Fatalf("unknown upstream result mislabeled: %+v %v", status, err)
	}
	options := f.s.opts
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s, err = Open(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	f.g.files["team/x.bin"] = fakeFile{object: api.Object{Path: "team/x.bin", Size: 2, SHA256: digest(data)}, data: data}
	status, err = f.s.Status(f.ctx(), u.TransferID)
	if err != nil || status.State != "uncertain" || status.Object == nil || status.Recovery == "" {
		t.Fatalf("recovery after restart: %+v %v", status, err)
	}
	if w := request(f.s, "PUT", u.UploadURL, data, u.Headers); w.Code != http.StatusConflict {
		t.Fatal("uncertain upload was retried")
	}
}

func TestEmptyFileDownloadUseLimitAndExpiry(t *testing.T) {
	f := newFixture(t)
	u := f.prepare(t, "team/empty.bin", nil)
	if w := request(f.s, "PUT", u.UploadURL, nil, u.Headers); w.Code != http.StatusCreated {
		t.Fatalf("empty upload: %d %s", w.Code, w.Body.String())
	}
	d, err := f.s.PrepareDownload(f.ctx(), "team/empty.bin")
	if err != nil {
		t.Fatal(err)
	}
	for range 32 {
		if w := request(f.s, "GET", d.DownloadURL, nil, nil); w.Code != http.StatusOK || w.Body.Len() != 0 {
			t.Fatalf("empty download: %d %s", w.Code, w.Body.String())
		}
	}
	if w := request(f.s, "GET", d.DownloadURL, nil, nil); w.Code != http.StatusConflict {
		t.Fatal("download capability usage limit was ignored")
	}
	f.s.now = func() time.Time { return d.ExpiresAt.Add(time.Second) }
	if w := request(f.s, "GET", d.DownloadURL, nil, nil); w.Code != http.StatusGone {
		t.Fatal("expired browser download still worked")
	}
}

type revokeReader struct {
	reader io.Reader
	f      *fixture
	once   sync.Once
}

func (r *revokeReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		r.f.mu.Lock()
		delete(r.f.users, r.f.owner.Subject)
		r.f.mu.Unlock()
	})
	return r.reader.Read(p)
}

func TestGrantRevokedWhileReceivingBody(t *testing.T) {
	f := newFixture(t)
	data := []byte{1, 2}
	u := f.prepare(t, "team/revoked.bin", data)
	r := httptest.NewRequest("PUT", u.UploadURL, bytes.NewReader(data))
	r.Body = io.NopCloser(&revokeReader{reader: bytes.NewReader(data), f: f})
	for key, value := range u.Headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	f.s.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || f.g.uploads != 0 {
		t.Fatalf("upload survived revocation during body reception: %d, writes=%d", w.Code, f.g.uploads)
	}
}

func TestPendingQuotaAndDatabasePermissions(t *testing.T) {
	f := newFixture(t)
	for range 64 {
		f.prepare(t, "team/pending.bin", nil)
	}
	if _, err := f.s.PrepareUpload(f.ctx(), UploadRequest{Path: "team/pending.bin", Size: 0, SHA256: digest(nil)}); err == nil {
		t.Fatal("unbounded pending transfer creation")
	}
	stat, err := os.Stat(f.s.opts.DatabasePath)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("transfer database is not private")
	}
	stat, err = os.Stat(f.s.spool)
	if err != nil || stat.Mode().Perm() != 0700 {
		t.Fatal("transfer spool directory is not private")
	}
}
