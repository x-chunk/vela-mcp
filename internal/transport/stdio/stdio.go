// Package stdio owns only transport lifecycle. stdout is reserved for MCP JSON;
// diagnostics must be sent to stderr by the composition root.
package stdio

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func Run(ctx context.Context, server *mcp.Server) error {
	return server.Run(ctx, &mcp.StdioTransport{})
}
