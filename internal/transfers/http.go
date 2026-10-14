package transfers

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"go.xchunk.org/vela-mcp/internal/app"
	"go.xchunk.org/vela/api"
)

// Handler serves byte routes without general OAuth bearer authentication. Every
// request needs the narrowly bound capability and a still-active OAuth grant.
// Reverse proxies must redact Authorization and token query parameters in logs.
func (s *Service) Handler() http.Handler { return s }

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "transfers" || (parts[1] != "uploads" && parts[1] != "downloads") || len(parts[2]) != 43 {
		http.NotFound(w, r)
		return
	}
	kind := "upload"
	if parts[1] == "downloads" {
		kind = "download"
	}
	if (kind == "upload" && r.Method != http.MethodPut) || (kind == "download" && r.Method != http.MethodGet && r.Method != http.MethodHead) {
		if kind == "upload" {
			w.Header().Set("Allow", "PUT")
		} else {
			w.Header().Set("Allow", "GET, HEAD")
		}
		problem(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use the transfer's advertised HTTP method.")
		return
	}
	token := ""
	if kind == "download" {
		token = r.URL.Query().Get("token")
	} else if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if len(token) != 43 {
		problem(w, http.StatusUnauthorized, "invalid_capability", "A valid transfer capability is required.")
		return
	}
	v, err := s.repo.get(r.Context(), parts[2])
	if err != nil || v.Kind != kind || subtle.ConstantTimeCompare([]byte(v.Hash), []byte(capabilityHash(token))) != 1 {
		problem(w, http.StatusUnauthorized, "invalid_capability", "A valid transfer capability is required.")
		return
	}
	if s.now().Unix() >= v.ExpiresAt {
		problem(w, http.StatusGone, "transfer_expired", "Prepare a new transfer.")
		return
	}
	workspace, err := s.workspace(r.Context(), v.Subject)
	if err != nil {
		problem(w, http.StatusUnauthorized, "grant_inactive", "The transfer owner's authorization is no longer active.")
		return
	}
	// Bound concurrent spooling before a single-use upload is claimed, allowing
	// callers to retry transient load shedding with the same unused capability.
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		w.Header().Set("Retry-After", "2")
		problem(w, http.StatusServiceUnavailable, "transfer_busy", "Too many active binary transfers; retry shortly.")
		return
	}
	claimed, err := s.repo.claim(r.Context(), v, s.now().Unix())
	if err != nil {
		problem(w, http.StatusServiceUnavailable, "state_unavailable", "Transfer state is temporarily unavailable.")
		return
	}
	if !claimed {
		problem(w, http.StatusConflict, "transfer_used", "The upload was already claimed or the download use limit was reached. Inspect transfer status.")
		return
	}
	if kind == "upload" {
		s.receiveUpload(w, r, v, workspace)
	} else {
		s.sendDownload(w, r, v, workspace)
	}
}

func problem(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// finish uses its own short context so a disconnected caller cannot leave an
// otherwise completed or rejected transfer marked as in progress forever.
func (s *Service) finish(id, state string, receipt any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var b []byte
	var err error
	if receipt != nil {
		b, err = json.Marshal(receipt)
		if err != nil {
			return err
		}
	}
	return s.repo.finish(ctx, id, state, b, s.now().Unix())
}

// spoolVerified bounds disk consumption and verifies the entire representation
// before upstream mutation or download response headers are sent. RAM usage is
// constant. Files are private, transient, and removed even on client disconnect.
func (s *Service) spoolVerified(ctx context.Context, reader io.Reader, size int64, digest string) (*os.File, error) {
	f, err := os.CreateTemp(s.spool, "transfer-")
	if err != nil {
		return nil, err
	}
	remove := func() {
		name := f.Name()
		f.Close()
		os.Remove(name)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(&contextReader{ctx: ctx, reader: reader}, size+1))
	if err != nil || n != size {
		remove()
		return nil, &app.Invalid{Message: "body does not match the declared byte size"}
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), digest) {
		remove()
		return nil, &app.Invalid{Message: "body does not match the expected SHA-256"}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		remove()
		return nil, err
	}
	return f, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func removeSpool(f *os.File) {
	name := f.Name()
	f.Close()
	os.Remove(name)
}

func (s *Service) receiveUpload(w http.ResponseWriter, r *http.Request, v record, workspace *app.Workspace) {
	var binding app.StreamUpload
	if err := json.Unmarshal(v.Binding, &binding); err != nil {
		_ = s.finish(v.ID, "failed", nil)
		problem(w, http.StatusInternalServerError, "invalid_state", "The transfer record cannot be read.")
		return
	}
	if _, err := workspace.PrepareStreamUpload(binding.Path, binding.Size, binding.SHA256, binding.Metadata); err != nil || binding.Size > s.opts.MaxUploadBytes {
		_ = s.finish(v.ID, "failed", nil)
		problem(w, http.StatusForbidden, "transfer_forbidden", "The bound upload is no longer authorized or exceeds configured limits.")
		return
	}
	if r.ContentLength >= 0 && r.ContentLength != binding.Size {
		_ = s.finish(v.ID, "failed", nil)
		problem(w, http.StatusUnprocessableEntity, "size_mismatch", "Content-Length does not match the prepared size.")
		return
	}
	f, err := s.spoolVerified(r.Context(), r.Body, binding.Size, binding.SHA256)
	if err != nil {
		_ = s.finish(v.ID, "failed", nil)
		problem(w, http.StatusUnprocessableEntity, "body_mismatch", "The complete upload must match the prepared size and SHA-256.")
		return
	}
	defer removeSpool(f)
	// Recheck the grant after receiving a potentially slow body. This prevents
	// a capability revoked during upload from producing a new mutation.
	workspace, err = s.workspace(r.Context(), v.Subject)
	if err != nil || s.now().Unix() >= v.ExpiresAt {
		_ = s.finish(v.ID, "failed", nil)
		problem(w, http.StatusUnauthorized, "grant_inactive", "Authorization or the transfer expired while receiving the body.")
		return
	}
	result, err := workspace.UploadStream(r.Context(), binding, f)
	if err != nil {
		state := "uncertain"
		var validation *app.Invalid
		var upstream *api.Error
		if errors.As(err, &validation) || errors.As(err, &upstream) {
			state = "failed"
		}
		_ = s.finish(v.ID, state, nil)
		status := http.StatusBadGateway
		code, message := "upstream_failure", "Vela did not return a verified receipt. Inspect get_file_transfer_status before retrying."
		if errors.As(err, &upstream) {
			status, code, message = upstream.Status(), string(upstream.Code), upstream.Message
		}
		problem(w, status, code, message)
		return
	}
	if err := s.finish(v.ID, "succeeded", result); err != nil {
		problem(w, http.StatusServiceUnavailable, "receipt_unavailable", "The file may have committed, but its receipt could not be persisted. Inspect transfer status and the destination.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"transfer_id": v.ID, "state": "succeeded", "upload": result})
}

func (s *Service) sendDownload(w http.ResponseWriter, r *http.Request, v record, workspace *app.Workspace) {
	var obj api.Object
	if err := json.Unmarshal(v.Binding, &obj); err != nil || obj.Size < 0 || obj.Size > s.opts.MaxDownloadBytes {
		problem(w, http.StatusInternalServerError, "invalid_state", "The bound download exceeds configured limits or cannot be read.")
		return
	}
	body, err := workspace.OpenStreamDownload(r.Context(), obj)
	if err != nil {
		problem(w, http.StatusConflict, "source_changed", "The source changed or is no longer accessible. Prepare a new download.")
		return
	}
	defer body.Close()
	f, err := s.spoolVerified(r.Context(), body, obj.Size, obj.SHA256)
	if err != nil {
		problem(w, http.StatusBadGateway, "source_mismatch", "Vela's bytes did not match the prepared content version. Prepare a new download.")
		return
	}
	defer removeSpool(f)
	if _, err := s.workspace(r.Context(), v.Subject); err != nil || s.now().Unix() >= v.ExpiresAt {
		problem(w, http.StatusUnauthorized, "grant_inactive", "Authorization or the transfer expired while fetching the file.")
		return
	}
	if err := s.finish(v.ID, "succeeded", nil); err != nil {
		problem(w, http.StatusServiceUnavailable, "state_unavailable", "Download state is temporarily unavailable.")
		return
	}
	filename := path.Base(obj.Path)
	// Always use attachment disposition at this capability origin. This allows
	// arbitrary binary downloads without executing uploaded HTML as same-origin
	// content alongside the OAuth consent page.
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("ETag", `"sha256:`+obj.SHA256+`"`)
	w.Header().Set("X-Content-SHA256", obj.SHA256)
	// ServeContent implements single/multipart byte ranges, If-Range, and HEAD
	// over the verified seekable representation. No partial unverified upstream
	// content is returned to the client.
	http.ServeContent(w, r, filename, obj.UpdatedAt, f)
}
