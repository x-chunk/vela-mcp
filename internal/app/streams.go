package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"

	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela/api"
)

// Identity returns the already-resolved caller to application integrations.
// Credential is redacted by its String methods and must never be serialized.
func (w *Workspace) Identity() access.Identity { return w.identity }

// ErrStreamReceiptMismatch means the upstream may have committed, but its
// response cannot establish the result. Transports must report uncertainty.
var ErrStreamReceiptMismatch = errors.New("upstream upload receipt does not match the transfer")

// StreamUpload is the immutable business contract bound to an HTTP transfer.
// Metadata is normalized at preparation, including absolute expiry times. It
// carries no local path, credential, or replacement authority.
type StreamUpload struct {
	Path     string   `json:"path"`
	Size     int64    `json:"size"`
	SHA256   string   `json:"sha256"`
	Metadata Metadata `json:"metadata"`
}

// PrepareStreamUpload authorizes a create-only upload before a transport accepts
// its bytes. Relative metadata expiry is resolved now, rather than at redemption.
func (w *Workspace) PrepareStreamUpload(p string, size int64, digest string, m Metadata) (StreamUpload, error) {
	p, err := w.object(p, api.ScopeRW)
	if err != nil {
		return StreamUpload{}, err
	}
	if size < 0 || size > w.limits.MaxUploadBytes {
		return StreamUpload{}, invalid("size must be 0..%d", w.limits.MaxUploadBytes)
	}
	digest = strings.ToLower(digest)
	b, err := hex.DecodeString(digest)
	if err != nil || len(b) != sha256.Size {
		return StreamUpload{}, invalid("sha256 must contain exactly 64 hexadecimal characters")
	}
	o, err := m.options()
	if err != nil {
		return StreamUpload{}, err
	}
	if o.Visibility == "" {
		o.Visibility = api.Private
	}
	m.Visibility, m.Tags = o.Visibility, o.Tags
	if o.ExpiresAt != nil {
		m.ExpiresAt = o.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return StreamUpload{Path: p, Size: size, SHA256: digest, Metadata: m}, nil
}

// UploadStream consumes a bounded, rewindable stream supplied by the transport.
// It verifies the complete body before creating anything upstream, then Vela
// independently verifies SHA-256 while committing. The reader can be a private
// spool file or another stream; the application never chooses a filesystem path.
func (w *Workspace) UploadStream(ctx context.Context, binding StreamUpload, r ReadSeekCloser) (*UploadResult, error) {
	bound, err := w.PrepareStreamUpload(binding.Path, binding.Size, binding.SHA256, binding.Metadata)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(&ctxReader{ctx, r}, bound.Size+1))
	if err != nil || n != bound.Size {
		return nil, invalid("upload body does not match the declared size")
	}
	if hex.EncodeToString(h.Sum(nil)) != bound.SHA256 {
		return nil, invalid("upload body does not match the declared sha256")
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, invalid("upload stream cannot be rewound")
	}
	o, err := bound.Metadata.options()
	if err != nil {
		return nil, err
	}
	o.Size, o.SHA256, o.CreateOnly = bound.Size, bound.SHA256, true
	obj, created, err := w.gateway.Upload(ctx, bound.Path, r, o)
	if err != nil {
		return nil, err
	}
	if obj == nil || obj.Path != bound.Path || obj.Size != bound.Size || !strings.EqualFold(obj.SHA256, bound.SHA256) {
		return nil, ErrStreamReceiptMismatch
	}
	return &UploadResult{Object: obj, Created: created}, nil
}

// OpenStreamDownload reauthorizes the object and binds downloads to one content
// version. The HTTP adapter verifies size and checksum before exposing any bytes,
// covering a replacement racing between this metadata check and the REST read.
func (w *Workspace) OpenStreamDownload(ctx context.Context, expected api.Object) (io.ReadCloser, error) {
	obj, err := w.Stat(ctx, expected.Path)
	if err != nil {
		return nil, err
	}
	if obj.Size < 0 || obj.Size > w.limits.MaxDownloadBytes {
		return nil, invalid("file exceeds configured max_download_bytes")
	}
	if obj.ID != expected.ID || obj.Revision != expected.Revision || obj.Size != expected.Size || !strings.EqualFold(obj.SHA256, expected.SHA256) {
		return nil, invalid("source changed after this transfer was prepared; prepare a new download")
	}
	return w.gateway.Download(ctx, obj.Path)
}
