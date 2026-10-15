package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela/api"
)

const (
	searchDefaultLimit = 20
	searchMaxLimit     = 100
	searchMaxPages     = 5
	searchMaxScanned   = 500
	searchMaxCursor    = 8 << 10
)

// SearchOptions selects a bounded metadata search. Query is a literal,
// case-insensitive substring; file contents are never downloaded or searched.
type SearchOptions struct {
	Query      string
	Prefix     string
	Cursor     string
	Visibility []api.Visibility
	Limit      int
}

// SearchResult reports matches and the progress of this invocation. An empty
// Files slice proves absence only when Complete is true. NextCursor resumes the
// same search; it does not represent a snapshot of a concurrently changing store.
type SearchResult struct {
	Files      []api.Object `json:"files"`
	Prefix     string       `json:"prefix"`
	Scanned    int          `json:"scanned"`
	Complete   bool         `json:"complete"`
	NextCursor string       `json:"next_cursor"`
}

// searchCursor wraps, but never interprets, Vela's opaque cursor. The binding
// prevents accidental reuse with a different search or caller; it is not an
// authorization token. Every resumed invocation still uses the caller's key.
type searchCursor struct {
	Version  int    `json:"v"`
	Upstream string `json:"upstream"`
	Binding  string `json:"binding"`
}

// Search examines at most five metadata pages and 500 objects. Each upstream
// page fits the remaining result slots, so every fetched row can be consumed
// before exposing its cursor. Returning the cursor from a partly consumed page
// would silently discard matches; decoding Vela's cursor would couple layers.
func (w *Workspace) Search(ctx context.Context, o SearchOptions) (*SearchResult, error) {
	query := strings.TrimSpace(o.Query)
	if !utf8.ValidString(query) || utf8.RuneCountInString(query) < 1 || utf8.RuneCountInString(query) > 256 {
		return nil, invalid("query must contain 1..256 UTF-8 characters after trimming whitespace")
	}
	query = strings.ToLower(query)
	if o.Limit == 0 {
		o.Limit = searchDefaultLimit
	}
	if o.Limit < 1 || o.Limit > searchMaxLimit {
		return nil, invalid("limit must be 1..100 or omitted")
	}
	prefix, err := w.directory(o.Prefix, api.ScopeRO, true)
	if err != nil {
		return nil, err
	}
	visibility, err := searchVisibility(o.Visibility)
	if err != nil {
		return nil, err
	}
	binding := w.searchBinding(query, prefix, visibility)
	cursor, err := decodeSearchCursor(o.Cursor, binding)
	if err != nil {
		return nil, err
	}

	out := &SearchResult{Files: []api.Object{}, Prefix: prefix}
	seen := map[string]bool{}
	lastPath := ""
	for pageNumber := 0; pageNumber < searchMaxPages && out.Scanned < searchMaxScanned && len(out.Files) < o.Limit; pageNumber++ {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if seen[cursor] {
			return nil, invalid("Vela returned a repeated search cursor")
		}
		seen[cursor] = true
		limit := min(searchMaxLimit, o.Limit-len(out.Files), searchMaxScanned-out.Scanned)
		page, err := w.gateway.List(ctx, ListOptions{Prefix: prefix, Recursive: true, Visibility: visibility, Limit: limit, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		if page == nil || len(page.Dirs) != 0 || len(page.Objects) > limit {
			return nil, invalid("Vela returned an invalid or oversized recursive search page")
		}
		if len(page.Objects) == 0 && page.NextCursor != "" {
			return nil, invalid("Vela returned a search cursor without making progress")
		}
		if page.NextCursor != "" && seen[page.NextCursor] {
			return nil, invalid("Vela returned a repeated search cursor")
		}
		for _, obj := range page.Objects {
			path, pathErr := api.CleanObjectPath(obj.Path)
			if pathErr != nil || path != obj.Path || obj.Path <= lastPath {
				return nil, invalid("Vela returned invalid or unordered search paths")
			}
			lastPath = obj.Path
			out.Scanned++
			// Future resolvers may narrow a broad upstream key. Do not trust
			// a gateway's listing filter to enforce that narrower authority.
			if !strings.HasPrefix(obj.Path, prefix) || access.Require(w.identity, api.ScopeRO, obj.Path) != nil {
				continue
			}
			if len(visibility) != 0 && !hasSearchVisibility(visibility, obj.Visibility) {
				continue
			}
			if matchesSearch(obj, query) {
				out.Files = append(out.Files, obj)
			}
		}
		cursor = page.NextCursor
		if cursor == "" {
			out.Complete = true
			return out, nil
		}
		if !utf8.ValidString(cursor) || len(cursor) > searchMaxCursor {
			return nil, invalid("Vela returned an oversized or invalid search cursor")
		}
	}
	out.NextCursor, err = encodeSearchCursor(cursor, binding)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func searchVisibility(values []api.Visibility) ([]api.Visibility, error) {
	seen := map[api.Visibility]bool{}
	var result []api.Visibility
	for _, value := range values {
		if !value.Valid() {
			return nil, invalid("visibility must be public, unlisted or private")
		}
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func hasSearchVisibility(values []api.Visibility, value api.Visibility) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func matchesSearch(obj api.Object, query string) bool {
	for _, field := range []string{obj.Path, obj.Title, obj.Description} {
		if strings.Contains(strings.ToLower(field), query) {
			return true
		}
	}
	for _, tag := range obj.Tags {
		if strings.Contains(strings.ToLower(tag), query) {
			return true
		}
	}
	return false
}

func (w *Workspace) searchBinding(query, prefix string, visibility []api.Visibility) string {
	// Struct fields give a stable encoding. Limit is intentionally excluded:
	// callers can request a smaller page without restarting the search.
	b, _ := json.Marshal(struct {
		Query, Prefix, Subject, Scope, AuthorityPrefix string
		Visibility                                     []api.Visibility
	}{query, prefix, w.identity.Subject, string(w.identity.Scope), w.identity.Prefix, visibility})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func decodeSearchCursor(value, binding string) (string, error) {
	if value == "" {
		return "", nil
	}
	if len(value) > searchMaxCursor {
		return "", invalid("search cursor exceeds 8 KiB; restart the search")
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return "", invalid("invalid search cursor; restart the search")
	}
	var c searchCursor
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err != nil {
		return "", invalid("invalid search cursor; restart the search")
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) || c.Version != 1 || c.Upstream == "" || c.Binding != binding {
		return "", invalid("search cursor does not match this query, filters or caller; restart the search")
	}
	return c.Upstream, nil
}

func encodeSearchCursor(upstream, binding string) (string, error) {
	if upstream == "" {
		return "", invalid("Vela did not provide a continuation for an incomplete search")
	}
	b, _ := json.Marshal(searchCursor{Version: 1, Upstream: upstream, Binding: binding})
	encoded := base64.RawURLEncoding.EncodeToString(b)
	if len(encoded) > searchMaxCursor {
		return "", invalid("Vela returned an oversized search cursor")
	}
	return encoded, nil
}
