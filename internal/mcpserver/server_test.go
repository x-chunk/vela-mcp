package mcpserver

import (
	"context"
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

func connect(t *testing.T, s *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	a, b := mcp.NewInMemoryTransports()
	server, err := s.Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	session, err := c.Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(); server.Close() })
	return session
}

type testFactory struct{ app.Gateway }

func (f testFactory) Open(context.Context, access.Credential) (app.Gateway, error) {
	return f.Gateway, nil
}

func TestHTTPResultBudgetPreservesStructuredDataOrReturnsActionableError(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "budget-test"}, nil)
	svc := &app.Service{Resolver: access.Static{Identity: access.Identity{Subject: "test", Scope: api.ScopeRO, Credential: access.NewCredential("dummy")}}, Gateways: testFactory{}}
	r := &registrar{server: s, service: svc, enabled: map[string]bool{"read_file": true}, timeout: time.Second, maxOutputBytes: 1024}
	register(r, "read_file", "test preview", object(schema{"path": str("preview")}, "path"), func(_ context.Context, _ *app.Workspace, a pathArgs) (any, error) {
		if a.Path == "large" {
			return map[string]any{"content": strings.Repeat("x", 1024)}, nil
		}
		return map[string]any{"content": "small"}, nil
	})
	session := connect(t, s)
	large, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_file", Arguments: map[string]any{"path": "large"}})
	if err != nil || !large.IsError {
		t.Fatal("oversized response was returned")
	}
	message := large.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(message, "max_bytes") || strings.Contains(message, "prepare_file_download") {
		t.Fatal("response limit did not explain recovery with the available tools")
	}
	small, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_file", Arguments: map[string]any{"path": "small"}})
	if err != nil || small.IsError || small.StructuredContent == nil {
		t.Fatal("bounded structured output was lost")
	}
}
func TestToolInventorySchemasAndDisabledCalls(t *testing.T) {
	all := catalog.Defaults()
	for name := range all {
		all[name] = true
	}
	// Instantiating all tools also resolves/validates every JSON schema.
	session := connect(t, NewWithTransfers(&app.Service{}, all, time.Second, &transfers.Service{}))
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != len(catalog.Tools()) {
		t.Fatalf("registered %d of %d tools", len(listed.Tools), len(catalog.Tools()))
	}
	for _, tool := range listed.Tools {
		if tool.InputSchema == nil || tool.Annotations == nil {
			t.Fatal("missing schema or risk annotation")
		}
	}
	defaults := catalog.Defaults()
	defaults["read_file"] = false
	session = connect(t, New(&app.Service{}, defaults, time.Second))
	listed, err = session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if !defaults[tool.Name] {
			t.Fatalf("disabled tool %s still discoverable", tool.Name)
		}
	}
	for _, name := range []string{"read_file", "replace_file", "empty_trash", "update_root_directory_policy"} {
		if res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}}); err == nil && (res == nil || !res.IsError) {
			t.Fatalf("disabled tool %s callable", name)
		}
	}
}
