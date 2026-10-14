package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela/api"
)

type fakeGateway struct {
	Gateway
	opts   UploadOptions
	data   string
	called bool
	links  []api.Link
}

func (g *fakeGateway) ListLinks(context.Context, LinkOptions) (*api.LinkList, error) {
	return &api.LinkList{Links: g.links}, nil
}

func TestLinkPaginationAfterAuthorityFilter(t *testing.T) {
	g := &fakeGateway{links: []api.Link{{ID: "a", Path: "other/a"}, {ID: "b", Path: "team/b"}, {ID: "c", Path: "team/c"}}}
	w := workspace(g, api.ScopeRW, "team/")
	page, err := w.Links(context.Background(), LinkOptions{Limit: 1})
	if err != nil || page.Total != 2 || page.NextOffset == nil || *page.NextOffset != 1 || page.Links[0].ID != "b" {
		t.Fatal("pagination or prefix filtering failed")
	}
	page, err = w.Links(context.Background(), LinkOptions{Limit: 1, Offset: 1})
	if err != nil || page.NextOffset != nil || page.Links[0].ID != "c" {
		t.Fatal("final link page incorrect")
	}
}

func (g *fakeGateway) Upload(_ context.Context, p string, r io.Reader, o UploadOptions) (*api.Object, bool, error) {
	g.called = true
	g.opts = o
	b, err := io.ReadAll(r)
	g.data = string(b)
	return &api.Object{Path: p, SHA256: o.SHA256, Visibility: o.Visibility, Revision: 1}, true, err
}
func (g *fakeGateway) Stat(_ context.Context, p string) (*api.Object, error) {
	h := sha256.Sum256([]byte(g.data))
	return &api.Object{Path: p, SHA256: hex.EncodeToString(h[:]), Size: int64(len(g.data))}, nil
}
func (g *fakeGateway) Download(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(g.data)), nil
}

type fakeFactory struct {
	g    Gateway
	seen []string
}

func (f *fakeFactory) Open(_ context.Context, c access.Credential) (Gateway, error) {
	f.seen = append(f.seen, c.Token())
	return f.g, nil
}

type requestResolver struct{}
type identityKey struct{}

func (requestResolver) Resolve(ctx context.Context) (access.Identity, error) {
	return ctx.Value(identityKey{}).(access.Identity), nil
}
func workspace(g Gateway, scope api.Scope, prefix string) *Workspace {
	return &Workspace{identity: access.Identity{Scope: scope, Prefix: prefix}, gateway: g, limits: Limits{MaxUploadBytes: 1024, MaxInlineBytes: 1024, MaxReadBytes: 1024}}
}

func TestCreateAndReplacementPolicy(t *testing.T) {
	g := &fakeGateway{}
	w := workspace(g, api.ScopeRW, "")
	text := "Привет"
	title := "Заголовок"
	result, err := w.Upload(context.Background(), "notes/hello.txt", Source{Text: &text}, Metadata{Title: &title, Tags: []string{"TAG"}}, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !g.opts.CreateOnly || g.opts.Visibility != api.Private || g.opts.Title == nil || *g.opts.Title != title || g.opts.Tags[0] != "tag" || g.opts.Size != int64(len(text)) || len(result.Object.SHA256) != 64 {
		t.Fatal("creation defaults or metadata lost")
	}
	g.called = false
	if _, err = w.Upload(context.Background(), "notes/hello.txt", Source{Text: &text}, Metadata{}, true, 0); err == nil || g.called {
		t.Fatal("replacement without revision reached backend")
	}
	if _, err = w.Upload(context.Background(), "notes/hello.txt", Source{Text: &text}, Metadata{}, true, 3); err != nil {
		t.Fatal(err)
	}
	if g.opts.CreateOnly || g.opts.IfRevision != 3 || g.opts.Visibility != "" {
		t.Fatal("replacement must preserve omitted metadata")
	}
	g.called = false
	if _, err = w.Upload(context.Background(), "notes/hello.txt", Source{Text: &text, Base64: &text}, Metadata{}, false, 0); err == nil || g.called {
		t.Fatal("ambiguous source accepted")
	}
	for _, w := range []*Workspace{workspace(g, api.ScopeRO, ""), workspace(g, api.ScopeRW, "other/")} {
		g.called = false
		if _, err = w.Upload(context.Background(), "notes/hello.txt", Source{Text: &text}, Metadata{}, false, 0); err == nil || g.called {
			t.Fatal("unauthorized write reached backend")
		}
	}
}
func TestReadUTF8BoundariesAndBinary(t *testing.T) {
	g := &fakeGateway{data: "A😀B"}
	w := workspace(g, api.ScopeRO, "")
	r, err := w.Read(context.Background(), "x", "auto", 3)
	if err != nil || r.Content != "A" || !r.Truncated || r.Encoding != "utf-8" {
		t.Fatalf("broken UTF-8 prefix: %+v %v", r, err)
	}
	g.data = string([]byte{255, 255, 255, 255})
	r, err = w.Read(context.Background(), "x", "auto", 2)
	if err != nil || r.Encoding != "base64" || r.BytesReturned != 2 {
		t.Fatal("binary tail was mistaken for truncated text")
	}
	if _, err = w.Read(context.Background(), "x", "utf-8", 2); err == nil {
		t.Fatal("binary returned as UTF-8")
	}
}
func TestIdentityResolvedPerInvocation(t *testing.T) {
	f := &fakeFactory{g: &fakeGateway{}}
	s := &Service{Resolver: requestResolver{}, Gateways: f}
	for _, secret := range []string{"first-dummy", "second-dummy"} {
		i := access.Identity{Subject: secret, Scope: api.ScopeRO, Credential: access.NewCredential(secret)}
		if _, err := s.Open(context.WithValue(context.Background(), identityKey{}, i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.seen) != 2 || f.seen[0] == f.seen[1] {
		t.Fatal("identity was cached across callers")
	}
}
