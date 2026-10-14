package mcpserver

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.xchunk.org/vela-mcp/internal/app"
	"go.xchunk.org/vela-mcp/internal/catalog"
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
func TestToolInventorySchemasAndDisabledCalls(t *testing.T) {
	all := catalog.Defaults()
	for name := range all {
		all[name] = true
	}
	// Instantiating all tools also resolves/validates every JSON schema.
	session := connect(t, New(&app.Service{}, all, time.Second))
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
