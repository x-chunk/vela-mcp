// Package localfiles supplies opt-in filesystem access using os.Root.
// Root-relative operations prevent symlink and traversal escapes, including
// races. No arbitrary filesystem access is exposed to model-provided paths.
package localfiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.xchunk.org/vela-mcp/internal/app"
)

var ErrAccess = errors.New("local path must be inside a configured accessible directory; upload must be a regular file; download target must not exist")

type root struct {
	path string
	dir  *os.Root
}
type Files struct{ uploads, downloads []root }

func New(uploadRoots, downloadRoots []string) (*Files, error) {
	f := &Files{}
	open := func(paths []string) ([]root, error) {
		var out []root
		for _, p := range paths {
			abs, err := filepath.Abs(p)
			if err != nil {
				return nil, ErrAccess
			}
			r, err := os.OpenRoot(abs)
			if err != nil {
				for _, v := range out {
					v.dir.Close()
				}
				return nil, ErrAccess
			}
			out = append(out, root{abs, r})
		}
		return out, nil
	}
	var err error
	f.uploads, err = open(uploadRoots)
	if err != nil {
		return nil, err
	}
	f.downloads, err = open(downloadRoots)
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func (f *Files) Close() {
	for _, r := range append(append([]root{}, f.uploads...), f.downloads...) {
		r.dir.Close()
	}
}
func selectRoot(roots []root, p string) (*os.Root, string, error) {
	if !filepath.IsAbs(p) {
		return nil, "", ErrAccess
	}
	abs := filepath.Clean(p)
	for _, r := range roots {
		rel, err := filepath.Rel(r.path, abs)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return r.dir, rel, nil
		}
	}
	return nil, "", ErrAccess
}
func (f *Files) OpenUpload(p string, maxBytes int64) (app.ReadSeekCloser, int64, error) {
	r, rel, err := selectRoot(f.uploads, p)
	if err != nil {
		return nil, 0, err
	}
	// Stat through the same root before opening to reject obvious devices/FIFOs.
	// O_NONBLOCK below also prevents a raced-in FIFO from hanging the server.
	file, err := openRegular(r, rel)
	if err != nil {
		return nil, 0, ErrAccess
	}
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() {
		file.Close()
		return nil, 0, ErrAccess
	}
	if st.Size() > maxBytes {
		file.Close()
		return nil, 0, errors.New("upload exceeds configured max_upload_bytes")
	}
	return file, st.Size(), nil
}

// SaveDownload creates with O_EXCL and removes an incomplete file on ANY error.
// It verifies SHA256 before returning success and never overwrites a local file.
func (f *Files) SaveDownload(ctx context.Context, p string, src io.Reader, maxBytes int64, expectedSHA string) (n int64, err error) {
	r, rel, err := selectRoot(f.downloads, p)
	if err != nil {
		return 0, err
	}
	dst, err := r.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 0, ErrAccess
	}
	defer func() {
		if err != nil {
			dst.Close()
			r.Remove(rel)
		}
	}()
	h := sha256.New()
	n, err = io.Copy(io.MultiWriter(dst, h), io.LimitReader(&contextReader{ctx, src}, maxBytes+1))
	if err != nil {
		return n, errors.New("download interrupted")
	}
	if n > maxBytes {
		return n, errors.New("download exceeds configured max_download_bytes")
	}
	if hex.EncodeToString(h.Sum(nil)) != expectedSHA {
		return n, errors.New("download checksum mismatch; source may have changed")
	}
	if err = dst.Sync(); err != nil {
		return n, errors.New("cannot persist download")
	}
	if err = dst.Close(); err != nil {
		return n, errors.New("cannot close download")
	}
	return n, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
