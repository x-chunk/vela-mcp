package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"mime"
	"time"
	"unicode/utf8"

	"go.xchunk.org/vela/api"
)

// Metadata is creation/replacement input. Nil leaves a replacement field alone.
// New files default to PRIVATE explicitly, regardless of Vela server defaults.
type Metadata struct {
	Visibility  api.Visibility  `json:"visibility,omitempty"`
	Title       *string         `json:"title,omitempty"`
	Description *string         `json:"description,omitempty"`
	Tags        []string        `json:"tags,omitempty"`
	ContentType string          `json:"content_type,omitempty"`
	Disposition api.Disposition `json:"disposition,omitempty"`
	ExpiresAt   string          `json:"expires_at,omitempty"`
}

func (m Metadata) options() (UploadOptions, error) {
	o := UploadOptions{ContentType: m.ContentType, Visibility: m.Visibility, Title: m.Title, Description: m.Description, Tags: m.Tags, Disposition: m.Disposition}
	if m.Visibility != "" && !m.Visibility.Valid() {
		return o, invalid("visibility must be public, unlisted or private")
	}
	if m.Disposition != "" && !m.Disposition.Valid() {
		return o, invalid("disposition must be inline or attachment")
	}
	if m.Title != nil {
		if err := api.ValidateTitle(*m.Title); err != nil {
			return o, invalid("%s", err)
		}
	}
	if m.Description != nil {
		if err := api.ValidateDescription(*m.Description); err != nil {
			return o, invalid("%s", err)
		}
	}
	if m.Tags != nil {
		tags, err := api.NormalizeTags(m.Tags)
		if err != nil {
			return o, invalid("%s", err)
		}
		o.Tags = tags
	}
	if m.ContentType != "" {
		if _, _, err := mime.ParseMediaType(m.ContentType); err != nil {
			return o, invalid("content_type must be a valid MIME type")
		}
	}
	if m.ExpiresAt != "" {
		t, err := api.ParseExpiry(m.ExpiresAt, time.Now())
		if err != nil {
			return o, invalid("%s", err)
		}
		if t != nil && !t.After(time.Now()) {
			return o, invalid("expires_at must be in the future")
		}
		o.ExpiresAt = t
		o.ClearExpiry = t == nil
	}
	return o, nil
}

// Source distinguishes an empty text/base64 file from an omitted input. Exactly
// one member must be present. Paths are process/container paths, not host URLs.
type Source struct {
	Text      *string
	Base64    *string
	LocalPath string
}

func (w *Workspace) source(s Source) (ReadSeekCloser, int64, error) {
	count := 0
	if s.Text != nil {
		count++
	}
	if s.Base64 != nil {
		count++
	}
	if s.LocalPath != "" {
		count++
	}
	if count != 1 {
		return nil, 0, invalid("provide exactly one of text, content_base64 or local_path")
	}
	if s.LocalPath != "" {
		if w.files == nil {
			return nil, 0, invalid("local file access is not configured")
		}
		r, n, err := w.files.OpenUpload(s.LocalPath, w.limits.MaxUploadBytes)
		if err != nil {
			return nil, 0, invalid("local source must be a regular readable file in upload_roots within max_upload_bytes")
		}
		return r, n, nil
	}
	var b []byte
	if s.Text != nil {
		if !utf8.ValidString(*s.Text) {
			return nil, 0, invalid("text must be valid UTF-8")
		}
		if int64(len(*s.Text)) > w.limits.MaxInlineBytes {
			return nil, 0, invalid("text exceeds configured max_inline_bytes; use local_path")
		}
		b = []byte(*s.Text)
	} else {
		if int64(len(*s.Base64)) > ((w.limits.MaxInlineBytes+2)/3)*4 {
			return nil, 0, invalid("base64 exceeds configured max_inline_bytes; use local_path")
		}
		var err error
		b, err = base64.StdEncoding.Strict().DecodeString(*s.Base64)
		if err != nil {
			return nil, 0, invalid("content_base64 must be standard padded base64 without a data URI prefix")
		}
	}
	if int64(len(b)) > w.limits.MaxInlineBytes {
		return nil, 0, invalid("content exceeds configured max_inline_bytes")
	}
	return &memorySource{bytes.NewReader(b)}, int64(len(b)), nil
}

type memorySource struct{ *bytes.Reader }

func (*memorySource) Close() error { return nil }

type UploadResult struct {
	Object  *api.Object `json:"object"`
	Created bool        `json:"created"`
}

func (w *Workspace) Upload(ctx context.Context, p string, source Source, m Metadata, replace bool, rev int64) (*UploadResult, error) {
	p, err := w.object(p, api.ScopeRW)
	if err != nil {
		return nil, err
	}
	if err = revision(rev); err != nil {
		return nil, err
	}
	if replace && rev == 0 {
		return nil, invalid("replace_file requires a positive expected_revision from get_file_metadata")
	}
	o, err := m.options()
	if err != nil {
		return nil, err
	}
	o.CreateOnly = !replace
	o.IfRevision = rev
	if !replace && o.Visibility == "" {
		o.Visibility = api.Private
	}
	if source.Text != nil && o.ContentType == "" {
		o.ContentType = "text/plain; charset=utf-8"
	}
	r, size, err := w.source(source)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	o.Size = size
	// Hash before upload. Vela verifies the bytes while streaming; this also
	// enables the client's conditional-upload recovery after a lost response.
	h := sha256.New()
	n, hashErr := io.Copy(h, io.LimitReader(&ctxReader{ctx, r}, size+1))
	if hashErr != nil || n != size {
		return nil, invalid("cannot read upload source")
	}
	o.SHA256 = hex.EncodeToString(h.Sum(nil))
	if _, err = r.Seek(0, io.SeekStart); err != nil {
		return nil, invalid("cannot rewind upload source")
	}
	obj, created, err := w.gateway.Upload(ctx, p, r, o)
	if err != nil {
		return nil, err
	}
	return &UploadResult{obj, created}, nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type ReadResult struct {
	Object        *api.Object `json:"object"`
	Encoding      string      `json:"encoding"`
	Content       string      `json:"content"`
	BytesReturned int         `json:"bytes_returned"`
	Truncated     bool        `json:"truncated"`
}

func (w *Workspace) Read(ctx context.Context, p, encoding string, maxBytes int64) (*ReadResult, error) {
	obj, err := w.Stat(ctx, p)
	if err != nil {
		return nil, err
	}
	if maxBytes == 0 {
		maxBytes = w.limits.MaxReadBytes
	}
	if maxBytes < 1 || maxBytes > w.limits.MaxReadBytes {
		return nil, invalid("max_bytes must be 1..%d", w.limits.MaxReadBytes)
	}
	if encoding == "" {
		encoding = "auto"
	}
	if encoding != "auto" && encoding != "utf-8" && encoding != "base64" {
		return nil, invalid("encoding must be auto, utf-8 or base64")
	}
	r, err := w.gateway.Download(ctx, obj.Path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(&ctxReader{ctx, r}, maxBytes+1))
	if err != nil {
		return nil, invalid("download interrupted")
	}
	truncated := int64(len(b)) > maxBytes
	if truncated {
		b = b[:maxBytes]
	}
	if !truncated {
		h := sha256.Sum256(b)
		if hex.EncodeToString(h[:]) != obj.SHA256 {
			return nil, invalid("source changed while reading; read it again")
		}
	}
	// If a prefix ends inside a UTF-8 rune, drop only the partial final rune.
	textBytes := b
	if truncated && len(textBytes) > 0 {
		start := len(textBytes) - 1
		for start > 0 && !utf8.RuneStart(textBytes[start]) {
			start--
		}
		if !utf8.FullRune(textBytes[start:]) && utf8.Valid(textBytes[:start]) {
			textBytes = textBytes[:start]
		}
	}
	if encoding == "auto" {
		if utf8.Valid(textBytes) && !bytes.Contains(textBytes, []byte{0}) {
			encoding = "utf-8"
		} else {
			encoding = "base64"
		}
	}
	content := ""
	if encoding == "utf-8" {
		if !utf8.Valid(textBytes) {
			return nil, invalid("content is not UTF-8; use encoding=base64")
		}
		b = textBytes
		content = string(b)
	} else {
		content = base64.StdEncoding.EncodeToString(b)
	}
	return &ReadResult{obj, encoding, content, len(b), truncated}, nil
}

type DownloadResult struct {
	Object       *api.Object `json:"object"`
	LocalPath    string      `json:"local_path"`
	BytesWritten int64       `json:"bytes_written"`
}

func (w *Workspace) Download(ctx context.Context, p, target string) (*DownloadResult, error) {
	obj, err := w.Stat(ctx, p)
	if err != nil {
		return nil, err
	}
	if obj.Size > w.limits.MaxDownloadBytes {
		return nil, invalid("file exceeds configured max_download_bytes")
	}
	if w.files == nil {
		return nil, invalid("local download directories are not configured")
	}
	r, err := w.gateway.Download(ctx, obj.Path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	n, err := w.files.SaveDownload(ctx, target, r, w.limits.MaxDownloadBytes, obj.SHA256)
	if err != nil {
		return nil, invalid("local download failed: %s", err)
	}
	return &DownloadResult{obj, target, n}, nil
}

func (w *Workspace) Patch(ctx context.Context, p string, patch api.ObjectPatch, rev int64) (*api.Object, error) {
	p, err := w.object(p, api.ScopeRW)
	if err != nil {
		return nil, err
	}
	if err = revision(rev); err != nil {
		return nil, err
	}
	if patch.IsEmpty() {
		return nil, invalid("patch must contain at least one metadata field")
	}
	m := Metadata{}
	if patch.Visibility.HasValue() {
		m.Visibility = patch.Visibility.Value
	}
	if patch.Title.HasValue() {
		m.Title = &patch.Title.Value
	}
	if patch.Description.HasValue() {
		m.Description = &patch.Description.Value
	}
	if patch.Tags.HasValue() {
		m.Tags = patch.Tags.Value
	}
	if patch.ContentType.HasValue() {
		m.ContentType = patch.ContentType.Value
	}
	if patch.Disposition.HasValue() {
		m.Disposition = patch.Disposition.Value
	}
	if _, err = m.options(); err != nil {
		return nil, err
	}
	if patch.Visibility.HasValue() && m.Visibility == "" || patch.Disposition.HasValue() && m.Disposition == "" {
		return nil, invalid("visibility/disposition cannot be empty; use null to reset")
	}
	if patch.ExpiresAt.HasValue() && !patch.ExpiresAt.Value.After(time.Now()) {
		return nil, invalid("expires_at must be a future RFC3339 timestamp or null")
	}
	return w.gateway.Patch(ctx, p, patch, rev)
}
func (w *Workspace) Visibility(ctx context.Context, p string, v api.Visibility, rev int64) (*api.Object, error) {
	if !v.Valid() {
		return nil, invalid("visibility must be public, unlisted or private")
	}
	return w.Patch(ctx, p, api.ObjectPatch{Visibility: api.Some(v)}, rev)
}
