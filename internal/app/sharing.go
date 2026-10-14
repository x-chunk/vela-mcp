package app

import (
	"context"
	"strings"
	"time"

	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela/api"
)

func (w *Workspace) Share(ctx context.Context, p, ttl, expires, note string) (*api.Link, error) {
	p, err := w.object(p, api.ScopeRW)
	if err != nil {
		return nil, err
	}
	if ttl != "" && expires != "" {
		return nil, invalid("provide ttl or expires_at, not both")
	}
	if ttl != "" {
		d, err := api.ParseDuration(ttl)
		if err != nil || d <= 0 {
			return nil, invalid("ttl must be a positive Vela duration such as 2h or 7d")
		}
	}
	var at *time.Time
	if expires != "" {
		v, err := time.Parse(time.RFC3339, expires)
		if err != nil || !v.After(time.Now()) {
			return nil, invalid("expires_at must be a future RFC3339 timestamp")
		}
		at = &v
	}
	return w.gateway.CreateLink(ctx, api.CreateLinkRequest{Path: p, TTL: ttl, ExpiresAt: at, Note: note})
}

// LinkPage bounds MCP output for Vela's unpaginated links endpoint. Offset pages
// are a fresh view, so callers should deduplicate IDs if links change meanwhile.
type LinkPage struct {
	Links      []api.Link `json:"links"`
	Total      int        `json:"total"`
	NextOffset *int       `json:"next_offset"`
}

func (w *Workspace) Links(ctx context.Context, o LinkOptions) (*LinkPage, error) {
	if err := access.Require(w.identity, api.ScopeRW); err != nil {
		return nil, err
	}
	if o.Path != "" {
		var err error
		o.Path, err = w.object(o.Path, api.ScopeRW)
		if err != nil {
			return nil, err
		}
	}
	if err := pageLimit(o.Limit); err != nil {
		return nil, err
	}
	if o.Offset < 0 {
		return nil, invalid("offset must not be negative")
	}
	if o.Limit == 0 {
		o.Limit = 200
	}
	page, err := w.gateway.ListLinks(ctx, o)
	if err != nil {
		return nil, err
	}
	if w.identity.Prefix != "" {
		out := &api.LinkList{Links: []api.Link{}}
		for _, l := range page.Links {
			if strings.HasPrefix(l.Path, w.identity.Prefix) {
				out.Links = append(out.Links, l)
			}
		}
		page = out
	}
	total := len(page.Links)
	start := min(o.Offset, total)
	end := start + min(o.Limit, total-start)
	out := &LinkPage{Links: page.Links[start:end], Total: total}
	if end < total {
		out.NextOffset = &end
	}
	return out, nil
}

// Revoke checks the link against an authorized object path. A share of a trashed
// file can be revoked by an unrestricted identity through the same ID check.
func (w *Workspace) Revoke(ctx context.Context, id, p string) error {
	if err := validID(id); err != nil {
		return err
	}
	if err := access.Require(w.identity, api.ScopeRW); err != nil {
		return err
	}
	if p != "" {
		var err error
		p, err = w.object(p, api.ScopeRW)
		if err != nil {
			return err
		}
	}
	if p == "" && w.identity.Prefix != "" {
		return invalid("path is required when revoking a link with a prefix-restricted identity")
	}
	links, err := w.gateway.ListLinks(ctx, LinkOptions{Path: p, IncludeExpired: true, IncludeRevoked: true})
	if err != nil {
		return err
	}
	for _, l := range links.Links {
		if l.ID == id && (p == "" || l.Path == p) {
			return w.gateway.RevokeLink(ctx, id)
		}
	}
	return invalid("link_id does not match an accessible signed link at path")
}
