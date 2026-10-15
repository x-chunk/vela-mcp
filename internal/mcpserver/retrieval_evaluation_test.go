package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela-mcp/internal/app"
	"go.xchunk.org/vela-mcp/internal/catalog"
	"go.xchunk.org/vela-mcp/internal/transfers"
	"go.xchunk.org/vela/api"
)

// These evaluations measure deterministic protocol bytes and calls, not model
// tokens or Claude accuracy. The legacy values freeze the pre-improvement DTO
// contract so improvements remain comparable on the same complete metadata.
func TestRetrievalPayloadEvaluation(t *testing.T) {
	t.Run("browse_50_files", func(t *testing.T) {
		g := newRetrievalFixture(200, strings.Repeat("x", 4096))
		session := retrievalSession(t, g)
		after := retrievalCall(t, session, "list_directory", map[string]any{"prefix": "reports/2026/"})
		data := retrievalData(t, after)
		rows, ok := data["objects"].([]any)
		if !ok || len(rows) != 50 || data["next_cursor"] == "" {
			t.Fatalf("default browsing must return 50 useful rows and continuation: %v", data)
		}
		first := rows[0].(map[string]any)
		for _, key := range []string{"path", "size", "content_type", "visibility", "revision"} {
			if _, ok := first[key]; !ok {
				t.Fatalf("compact listing lost %s", key)
			}
		}
		for _, key := range []string{"sha256", "id", "description", "created_at", "updated_at", "url"} {
			if _, ok := first[key]; ok {
				t.Fatalf("compact browsing still exposes unrelated %s", key)
			}
		}
		before := legacyRetrievalResult(t, &api.Listing{Prefix: "reports/2026/", Dirs: []string{}, Objects: g.objects[:50], NextCursor: "fixture-50"})
		beforeDefault := legacyRetrievalResult(t, &api.Listing{Prefix: "reports/2026/", Dirs: []string{}, Objects: g.objects, NextCursor: ""})
		beforeBytes, afterBytes := retrievalBytes(t, before), retrievalBytes(t, after)
		if afterBytes >= beforeBytes*60/100 {
			t.Fatalf("compact 50-row page did not remove substantial unrelated data: before=%d after=%d", beforeBytes, afterBytes)
		}
		if retrievalBytes(t, beforeDefault) <= 128000 {
			t.Fatal("legacy default fixture no longer reproduces the HTTP response limit failure")
		}
		t.Logf("browse same 50 rows: legacy_result_bytes=%d improved_result_bytes=%d upstream_calls=%d; legacy_default_200_result_bytes=%d", beforeBytes, afterBytes, g.calls.total(), retrievalBytes(t, beforeDefault))
	})

	for _, tc := range []struct {
		name, content string
	}{
		{"short_note", "A short note.\n"},
		{"markdown", strings.Repeat("A line of plain English text.\n", 128)},
		{"escaped_html", strings.Repeat("<", 32<<10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRetrievalFixture(1, tc.content)
			session := retrievalSession(t, g)
			after := retrievalCall(t, session, "read_file", map[string]any{"path": g.objects[0].Path})
			data := retrievalData(t, after)
			content, ok := data["content"].(string)
			if !ok || content == "" || !strings.HasPrefix(tc.content, content) || data["path"] != g.objects[0].Path || data["revision"] != float64(7) {
				t.Fatalf("concise reading lost content, identity, or edit revision: %v", data)
			}
			if _, ok := data["object"]; ok {
				t.Fatal("concise read returned full metadata")
			}
			if data["bytes_returned"] != float64(len(content)) || data["truncated"] != (len(content) < len(tc.content)) {
				t.Fatal("read truncation or byte count is misleading")
			}
			before := legacyRetrievalResult(t, &app.ReadResult{Object: &g.objects[0], Encoding: "utf-8", Content: tc.content, BytesReturned: len(tc.content), Truncated: false})
			beforeBytes, afterBytes := retrievalBytes(t, before), retrievalBytes(t, after)
			if afterBytes >= beforeBytes || afterBytes > 128000 {
				t.Fatalf("read did not become smaller and remain within the HTTP budget: before=%d after=%d", beforeBytes, afterBytes)
			}
			t.Logf("read raw_bytes=%d: legacy_result_bytes=%d improved_result_bytes=%d returned_raw_bytes=%d truncated=%v upstream_calls=%d", len(tc.content), beforeBytes, afterBytes, len(content), data["truncated"], g.calls.total())
		})
	}
}

func TestFindThenReadWorkflowEvaluation(t *testing.T) {
	const query = "incident-key-742"
	baseline := newRetrievalFixture(200, "A short note.\n")
	baseline.objects[79].Title = "Incident summary " + query
	// The former tools required the client to inspect full listing pages to
	// discover a title match. Use 50 rows to avoid the former default's error.
	var beforeBytes, beforeMCPCalls int
	var target string
	for cursor := ""; target == ""; {
		page, err := baseline.List(context.Background(), app.ListOptions{Prefix: "reports/2026/", Recursive: true, Limit: 50, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		beforeMCPCalls++
		beforeBytes += retrievalBytes(t, legacyRetrievalResult(t, page))
		for _, file := range page.Objects {
			if strings.Contains(strings.ToLower(file.Title), query) {
				target = file.Path
				break
			}
		}
		if target == "" && page.NextCursor == "" {
			t.Fatal("baseline fixture has no matching file")
		}
		cursor = page.NextCursor
	}
	object, err := baseline.Stat(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	body, err := baseline.Download(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	beforeMCPCalls++
	beforeBytes += retrievalBytes(t, legacyRetrievalResult(t, &app.ReadResult{Object: object, Encoding: "utf-8", Content: string(content), BytesReturned: len(content)}))

	improved := newRetrievalFixture(200, "A short note.\n")
	improved.objects[79].Title = "Incident summary " + query
	session := retrievalSession(t, improved)
	search := retrievalCall(t, session, "search_files", map[string]any{"prefix": "reports/2026/", "query": strings.ToUpper(query)})
	data := retrievalData(t, search)
	files, ok := data["files"].([]any)
	if !ok || len(files) != 1 || files[0].(map[string]any)["path"] != target {
		t.Fatalf("metadata search did not find the case-insensitive title match: %v", data)
	}
	if _, ok := data["scanned"].(float64); !ok {
		t.Fatal("search did not disclose its bounded scanning work")
	}
	if complete, ok := data["complete"].(bool); !ok || (!complete && data["next_cursor"] == "") {
		t.Fatal("search did not distinguish a completed scan from a resumable partial scan")
	}
	read := retrievalCall(t, session, "read_file", map[string]any{"path": target})
	if retrievalData(t, read)["content"] != string(content) {
		t.Fatal("find-and-read returned different content")
	}
	afterBytes := retrievalBytes(t, search) + retrievalBytes(t, read)
	if afterBytes >= beforeBytes || beforeMCPCalls <= 2 {
		t.Fatalf("find-and-read did not reduce client interaction/context: legacy_calls=%d legacy_bytes=%d improved_bytes=%d", beforeMCPCalls, beforeBytes, afterBytes)
	}
	t.Logf("find then read: legacy_mcp_calls=%d improved_mcp_calls=2 legacy_result_bytes=%d improved_result_bytes=%d legacy_rest_calls=%d improved_rest_calls=%d (list=%d stat=%d download=%d)", beforeMCPCalls, beforeBytes, afterBytes, baseline.calls.total(), improved.calls.total(), improved.calls.list, improved.calls.stat, improved.calls.download)
}

type retrievalCalls struct{ list, stat, download int }

func (c retrievalCalls) total() int { return c.list + c.stat + c.download }

// retrievalFixture models the existing REST contract, including opaque cursor
// pages, rather than doing search or response shaping on behalf of the tool.
type retrievalFixture struct {
	app.Gateway
	objects []api.Object
	content string
	calls   retrievalCalls
}

func newRetrievalFixture(n int, content string) *retrievalFixture {
	g := &retrievalFixture{content: content}
	timestamp := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	sum := sha256.Sum256([]byte(content))
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("monthly-report-%03d.md", i+1)
		g.objects = append(g.objects, api.Object{
			ID: fmt.Sprintf("01ja000000000000000000%04d", i+1), Path: "reports/2026/" + name, Name: name,
			Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]), ContentType: "text/markdown; charset=utf-8",
			Disposition: api.Inline, Visibility: api.Private, Title: fmt.Sprintf("Monthly operations report %03d", i+1),
			Description: "Monthly operations summary covering service reliability, deployment changes, capacity planning, and follow-up actions.",
			Tags:        []string{"reports", "operations", "2026"}, CreatedAt: timestamp, UpdatedAt: timestamp, Revision: 7,
		})
	}
	return g
}

func (g *retrievalFixture) List(_ context.Context, o app.ListOptions) (*api.Listing, error) {
	g.calls.list++
	start := 0
	if o.Cursor != "" {
		if !strings.HasPrefix(o.Cursor, "fixture-") {
			return nil, fmt.Errorf("unknown fixture cursor %q", o.Cursor)
		}
		var err error
		start, err = strconv.Atoi(strings.TrimPrefix(o.Cursor, "fixture-"))
		if err != nil {
			return nil, err
		}
	}
	limit := o.Limit
	if limit == 0 {
		limit = 200 // Vela's upstream default before MCP-specific shaping.
	}
	var matches []api.Object
	for _, object := range g.objects {
		if !strings.HasPrefix(object.Path, o.Prefix) {
			continue
		}
		visible := len(o.Visibility) == 0
		for _, visibility := range o.Visibility {
			visible = visible || visibility == object.Visibility
		}
		if visible {
			matches = append(matches, object)
		}
	}
	if start < 0 || start > len(matches) {
		return nil, fmt.Errorf("fixture cursor offset outside listing")
	}
	end := min(start+limit, len(matches))
	page := &api.Listing{Prefix: o.Prefix, Dirs: []string{}, Objects: matches[start:end]}
	if end < len(matches) {
		page.NextCursor = fmt.Sprintf("fixture-%d", end)
	}
	return page, nil
}

func (g *retrievalFixture) Stat(_ context.Context, path string) (*api.Object, error) {
	g.calls.stat++
	for _, object := range g.objects {
		if object.Path == path {
			return &object, nil
		}
	}
	return nil, fmt.Errorf("fixture file not found")
}

func (g *retrievalFixture) Download(_ context.Context, path string) (io.ReadCloser, error) {
	g.calls.download++
	for _, object := range g.objects {
		if object.Path == path {
			return io.NopCloser(strings.NewReader(g.content)), nil
		}
	}
	return nil, fmt.Errorf("fixture file not found")
}

func retrievalSession(t *testing.T, g *retrievalFixture) *mcp.ClientSession {
	t.Helper()
	svc := &app.Service{
		Resolver: access.Static{Identity: access.Identity{Subject: "retrieval-evaluation", Scope: api.ScopeRO, Credential: access.NewCredential("fixture-only")}},
		Gateways: testFactory{g},
		Limits:   app.Limits{MaxReadBytes: 32 << 10, MaxInlineBytes: 1 << 20, MaxUploadBytes: 8 << 20, MaxDownloadBytes: 8 << 20},
	}
	return connect(t, NewWithTransfers(svc, catalog.Defaults(), time.Second, &transfers.Service{}, 128000))
}

func retrievalCall(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("%s failed: %v", name, result.Content)
	}
	return result
}

func retrievalData(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	data, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured result is %T", result.StructuredContent)
	}
	return data
}

func legacyRetrievalResult(t *testing.T, output any) *mcp.CallToolResult {
	t.Helper()
	serialized, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(serialized)}}, StructuredContent: output}
}

func retrievalBytes(t *testing.T, result *mcp.CallToolResult) int {
	t.Helper()
	serialized, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return len(serialized)
}
