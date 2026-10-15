package mcpserver

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela-mcp/internal/app"
	"go.xchunk.org/vela/api"
)

type sharingFixtureGateway struct {
	app.Gateway
	creates int
	limit   int
	opens   int
}

func (g *sharingFixtureGateway) Open(context.Context, access.Credential) (app.Gateway, error) {
	g.opens++
	return g, nil
}

func (g *sharingFixtureGateway) CreateLink(context.Context, api.CreateLinkRequest) (*api.Link, error) {
	g.creates++
	return &api.Link{}, nil
}

func (g *sharingFixtureGateway) ListLinks(_ context.Context, o app.LinkOptions) (*api.LinkList, error) {
	g.limit = o.Limit
	return &api.LinkList{Links: []api.Link{}}, nil
}

func sharingFixture(t *testing.T) (*mcp.ClientSession, *sharingFixtureGateway) {
	t.Helper()
	g := &sharingFixtureGateway{}
	s := &app.Service{
		Resolver: access.Static{Identity: access.Identity{Subject: "test", Scope: api.ScopeRW, Credential: access.NewCredential("dummy")}},
		Gateways: g,
	}
	return connect(t, New(s, nil, time.Second)), g
}

func TestShareExpirySchemaRejectsConflictingInputsBeforeMutation(t *testing.T) {
	session, gateway := sharingFixture(t)
	for _, fields := range []map[string]any{
		{"path": "docs/report.md"},
		{"path": "docs/report.md", "ttl": "2h"},
		{"path": "docs/report.md", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
	} {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_share_link", Arguments: fields})
		if err != nil || result.IsError {
			t.Fatalf("valid expiry choice rejected: %v, %+v", err, result)
		}
	}
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_share_link", Arguments: map[string]any{
		"path": "docs/report.md", "ttl": "2h", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}})
	if err == nil && (result == nil || !result.IsError) {
		t.Fatal("conflicting expiration fields were accepted")
	}
	if gateway.creates != 3 {
		t.Fatalf("invalid expiry choice reached mutation: creates=%d", gateway.creates)
	}
	if gateway.opens != 3 {
		t.Fatalf("invalid expiry choice reached the application despite schema: opens=%d", gateway.opens)
	}
}

func TestShareLinkListingUsesConcisePageDefault(t *testing.T) {
	session, gateway := sharingFixture(t)
	for _, tc := range []struct {
		args map[string]any
		want int
	}{
		{map[string]any{"path": "docs/report.md"}, 50},
		{map[string]any{"path": "docs/report.md", "limit": 7}, 7},
	} {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_share_links", Arguments: tc.args})
		if err != nil || result.IsError || gateway.limit != tc.want {
			t.Fatalf("listing limit=%d, want=%d; err=%v, result=%+v", gateway.limit, tc.want, err, result)
		}
	}
}
