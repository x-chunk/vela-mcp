package mcpserver

import (
	"encoding/base64"
	"encoding/json"
	"mime"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.xchunk.org/vela-mcp/internal/app"
	"go.xchunk.org/vela-mcp/internal/version"
	"go.xchunk.org/vela/api"
)

// Discovery keeps fields useful for choosing a file and safely editing its
// metadata. Hashes, descriptions, URLs and timestamps are available on demand.
type fileSummary struct {
	Path        string         `json:"path"`
	Size        int64          `json:"size"`
	ContentType string         `json:"content_type"`
	Visibility  api.Visibility `json:"visibility"`
	Revision    int64          `json:"revision"`
	Title       string         `json:"title,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
}

func summarizeFile(o api.Object) fileSummary {
	return fileSummary{o.Path, o.Size, o.ContentType, o.Visibility, o.Revision, o.Title, o.Tags}
}

func summarizeFiles(objects []api.Object) []fileSummary {
	out := make([]fileSummary, 0, len(objects))
	for _, o := range objects {
		out = append(out, summarizeFile(o))
	}
	return out
}

func listingResult(page *api.Listing, format string) any {
	if format == "detailed" {
		return page
	}
	return struct {
		Prefix     string        `json:"prefix"`
		Dirs       []string      `json:"dirs"`
		Objects    []fileSummary `json:"objects"`
		NextCursor string        `json:"next_cursor"`
	}{page.Prefix, page.Dirs, summarizeFiles(page.Objects), page.NextCursor}
}

func searchResult(page *app.SearchResult) any {
	return struct {
		Prefix     string        `json:"prefix"`
		Files      []fileSummary `json:"files"`
		Scanned    int           `json:"scanned"`
		Complete   bool          `json:"complete"`
		NextCursor string        `json:"next_cursor"`
	}{page.Prefix, summarizeFiles(page.Files), page.Scanned, page.Complete, page.NextCursor}
}

type readPreview struct {
	Path          string      `json:"path"`
	Revision      int64       `json:"revision"`
	Size          int64       `json:"size"`
	Encoding      string      `json:"encoding"`
	Content       string      `json:"content"`
	BytesReturned int         `json:"bytes_returned"`
	Truncated     bool        `json:"truncated"`
	Object        *api.Object `json:"object,omitempty"`
}

func binaryContentType(raw string) bool {
	t, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return false
	}
	if strings.HasSuffix(t, "+xml") {
		return false
	}
	if strings.HasPrefix(t, "image/") || strings.HasPrefix(t, "audio/") || strings.HasPrefix(t, "video/") || strings.HasPrefix(t, "font/") {
		return true
	}
	switch t {
	case "application/pdf", "application/octet-stream", "application/zip", "application/gzip", "application/x-7z-compressed", "application/x-rar-compressed", "application/vnd.rar", "application/x-bzip2", "application/x-xz", "application/msword":
		return true
	}
	// Unknown application/* types can still be text (shell, TOML, YAML).
	// The reader's UTF-8/NUL checks decide those instead of a narrow MIME allowlist.
	return false
}

// Both MCP representations count toward the budget. Raw-byte caps alone do not
// bound escaped JSON (notably HTML); shorten a preview rather than making the
// client download the same file again with a guessed smaller max_bytes.
func (r *registrar) readResult(v *app.ReadResult, format string) (any, error) {
	out := readPreview{Path: v.Object.Path, Revision: v.Object.Revision, Size: v.Object.Size, Encoding: v.Encoding, Content: v.Content, BytesReturned: v.BytesReturned, Truncated: v.Truncated}
	if format == "detailed" {
		out.Object = v.Object
	}
	if r.maxOutputBytes <= 0 || r.fits(out) {
		return out, nil
	}
	raw := []byte(v.Content)
	if v.Encoding == "base64" {
		var err error
		raw, err = base64.StdEncoding.DecodeString(v.Content)
		if err != nil {
			return nil, &app.Invalid{Message: "cannot encode binary preview"}
		}
	}
	setPrefix := func(n int) {
		b := raw[:n]
		if v.Encoding == "base64" {
			out.Content = base64.StdEncoding.EncodeToString(b)
		} else {
			for len(b) > 0 && !utf8.Valid(b) {
				b = b[:len(b)-1]
			}
			out.Content = string(b)
		}
		out.BytesReturned = len(b)
		out.Truncated = true
	}
	setPrefix(0)
	if !r.fits(out) {
		return nil, &app.Invalid{Message: "read metadata exceeds http.max_tool_result_bytes; use response_format=concise or increase the configured budget"}
	}
	low, high := 0, len(raw)
	for low < high {
		mid := low + (high-low+1)/2
		setPrefix(mid)
		if r.fits(out) {
			low = mid
		} else {
			high = mid - 1
		}
	}
	setPrefix(low)
	return out, nil
}

func encodedResult(out any) (*mcp.CallToolResult, int, error) {
	b, err := json.Marshal(out)
	if err != nil {
		return nil, 0, err
	}
	result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, StructuredContent: out}
	b, err = json.Marshal(result)
	if err != nil {
		return nil, 0, err
	}
	// The SDK adds these fields after the handler for the 2026 protocol.
	// Reserve their exact size also for older clients, whose results are smaller.
	envelope, err := json.Marshal(map[string]any{"_meta": map[string]any{mcp.MetaKeyServerInfo: &mcp.Implementation{Name: "vela-mcp", Version: version.Get().Version}}, "resultType": "complete"})
	return result, len(b) + len(envelope) - 1, err
}

func (r *registrar) fits(out any) bool {
	_, n, err := encodedResult(out)
	return err == nil && int64(n) <= r.maxOutputBytes
}

func responseFormat() schema {
	return enum("Default concise: only fields needed for this task. Detailed includes complete file metadata.", "concise", "detailed")
}
