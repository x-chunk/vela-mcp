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
	share := object(schema{"path": filePath(), "ttl": str("Positive Vela duration, e.g. 2h or 7d; bounded by server link_max_ttl."), "expires_at": str("Future RFC3339 timestamp; omit when ttl is supplied."), "note": str("Purpose/recipient note, stored with the link.")}, "path")
	share["not"] = schema{"required": []string{"ttl", "expires_at"}}
	register(r, "create_share_link", "Share a file through a temporary signed URL without changing visibility. Anyone with the URL can download it; links survive moves. Supply ttl or expires_at, or omit both for Vela's default lifetime.", share, func(ctx context.Context, w *app.Workspace, a shareArgs) (any, error) {
		return w.Share(ctx, a.Path, a.TTL, a.ExpiresAt, a.Note)
	})
	register(r, "list_share_links", "Find existing signed links, preferably for one path. Requires rw. Returns 50 links by default; use next_offset only if needed with unchanged filters. Deduplicate IDs if links change between pages. Returned URLs grant access.", object(schema{"path": filePath(), "include_expired": boolean("Include expired links; default false."), "include_revoked": boolean("Include revoked links; default false."), "limit": integer("Returned links per page, default 50.", 1, 1000), "offset": integer("next_offset from the preceding page; default 0. Pages are fresh views, not a snapshot.", 0, 0)}), func(ctx context.Context, w *app.Workspace, a linksArgs) (any, error) {
		if a.Limit == 0 {
			a.Limit = 50
		}
		return w.Links(ctx, app.LinkOptions{Path: a.Path, IncludeExpired: a.IncludeExpired, IncludeRevoked: a.IncludeRevoked, Limit: a.Limit, Offset: a.Offset})
	})
	register(r, "revoke_share_link", "Invalidate one signed link using its link_id from create_share_link or list_share_links. A prefix-restricted key also needs its current file path. An unrestricted key may omit path, including for trashed objects.", object(schema{"link_id": id(), "path": filePath()}, "link_id"), func(ctx context.Context, w *app.Workspace, a revokeArgs) (any, error) {
		return done(w.Revoke(ctx, a.LinkID, a.Path))
	})
}
