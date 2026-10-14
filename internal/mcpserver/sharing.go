package mcpserver

import (
	"context"

	"go.xchunk.org/vela-mcp/internal/app"
)

type shareArgs struct {
	Path      string `json:"path"`
	TTL       string `json:"ttl,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Note      string `json:"note,omitempty"`
}
type linksArgs struct {
	Path           string `json:"path,omitempty"`
	IncludeExpired bool   `json:"include_expired,omitempty"`
	IncludeRevoked bool   `json:"include_revoked,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	Offset         int    `json:"offset,omitempty"`
}
type revokeArgs struct {
	LinkID string `json:"link_id"`
	Path   string `json:"path,omitempty"`
}

func (r *registrar) sharing() {
	register(r, "create_share_link", "Create a temporary signed URL, including for a private file. The URL grants access to anyone who has it; it survives renames/moves. Use ttl OR expires_at, or omit both for Vela's default lifetime.", object(schema{"path": filePath(), "ttl": str("Positive Vela duration, e.g. 2h, 7d; bounded by server link_max_ttl."), "expires_at": str("Future RFC3339 timestamp; mutually exclusive with ttl."), "note": str("Purpose/recipient note, stored with the link.")}, "path"), func(ctx context.Context, w *app.Workspace, a shareArgs) (any, error) {
		return w.Share(ctx, a.Path, a.TTL, a.ExpiresAt, a.Note)
	})
	register(r, "list_share_links", "List signed URLs (rw required), bounded to 200 by default. Follow next_offset with the same filters for more; deduplicate IDs if links change between calls. URLs grant access; only share with intended recipients.", object(schema{"path": filePath(), "include_expired": boolean("Include expired links; default false."), "include_revoked": boolean("Include revoked links; default false."), "limit": integer("Returned links per page, default 200.", 1, 1000), "offset": integer("Offset from next_offset; default 0. Vela fetches the entire filtered list upstream.", 0, 0)}), func(ctx context.Context, w *app.Workspace, a linksArgs) (any, error) {
		return w.Links(ctx, app.LinkOptions{Path: a.Path, IncludeExpired: a.IncludeExpired, IncludeRevoked: a.IncludeRevoked, Limit: a.Limit, Offset: a.Offset})
	})
	register(r, "revoke_share_link", "Immediately invalidate one signed link by link_id. Supply its current file path for prefix-restricted keys. For unrestricted keys path may be omitted, including for trashed objects.", object(schema{"link_id": id(), "path": filePath()}, "link_id"), func(ctx context.Context, w *app.Workspace, a revokeArgs) (any, error) {
		return done(w.Revoke(ctx, a.LinkID, a.Path))
	})
}
