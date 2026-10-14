package app

import (
	"context"
	"fmt"
	"strings"

	"go.xchunk.org/vela-mcp/internal/access"
	"go.xchunk.org/vela/api"
)

// Service owns dependencies, not a caller identity. A Workspace is request
// scoped, preventing identity or credentials leaking between future HTTP users.
type Service struct {
	Resolver access.Resolver
	Gateways GatewayFactory
	Files    LocalFiles
	Limits   Limits
}
type Workspace struct {
	identity access.Identity
	gateway  Gateway
	files    LocalFiles
	limits   Limits
}

func (s *Service) Open(ctx context.Context) (*Workspace, error) {
	i, err := s.Resolver.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	prefix, err := api.CleanPrefix(i.Prefix)
	if err != nil || prefix != i.Prefix || !i.Scope.Valid() || i.Subject == "" || i.Credential.Token() == "" {
		return nil, access.ErrForbidden
	}
	g, err := s.Gateways.Open(ctx, i.Credential)
	if err != nil {
		return nil, err
	}
	return &Workspace{i, g, s.Files, s.Limits}, nil
}

// Invalid is a user-correctable validation error, safe to expose through MCP.
type Invalid struct{ Message string }

func (e *Invalid) Error() string               { return e.Message }
func invalid(format string, args ...any) error { return &Invalid{fmt.Sprintf(format, args...)} }
func cleanObject(p string) (string, error) {
	v, err := api.CleanObjectPath(p)
	if err != nil {
		return "", invalid("%s", err)
	}
	return v, nil
}
func cleanPrefix(p string) (string, error) {
	v, err := api.CleanPrefix(p)
	if err != nil {
		return "", invalid("%s", err)
	}
	return v, nil
}
func pageLimit(n int) error {
	if n < 0 || n > 1000 {
		return invalid("limit must be 1..1000 or omitted")
	}
	return nil
}
func revision(n int64) error {
	if n < 0 {
		return invalid("expected_revision must be positive or omitted")
	}
	return nil
}
func validID(id string) error {
	if len(id) != 26 {
		return invalid("id must be a lowercase Vela ULID")
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdefghjkmnpqrstvwxyz", r) {
			return invalid("id must be a lowercase Vela ULID")
		}
	}
	return nil
}
func (w *Workspace) object(p string, scope api.Scope) (string, error) {
	p, err := cleanObject(p)
	if err != nil {
		return "", err
	}
	if err = access.Require(w.identity, scope, p); err != nil {
		return "", err
	}
	return p, nil
}
func (w *Workspace) directory(p string, scope api.Scope, listing bool) (string, error) {
	p, err := cleanPrefix(p)
	if err != nil {
		return "", err
	}
	if listing {
		p, err = access.ListingPrefix(w.identity, p)
		if err != nil {
			return "", err
		}
	}
	if err = access.Require(w.identity, scope, p); err != nil {
		return "", err
	}
	return p, nil
}

func (w *Workspace) Info(ctx context.Context) (*api.Info, error) {
	if err := access.Require(w.identity, api.ScopeRO); err != nil {
		return nil, err
	}
	i, err := w.gateway.Info(ctx)
	if err != nil {
		return nil, err
	}
	// A narrower OAuth authority must not advertise a broader upstream scope.
	i.Key.Scope = w.identity.Scope
	i.Key.Prefix = w.identity.Prefix
	return i, nil
}
func (w *Workspace) List(ctx context.Context, o ListOptions) (*api.Listing, error) {
	p, err := w.directory(o.Prefix, api.ScopeRO, true)
	if err != nil {
		return nil, err
	}
	o.Prefix = p
	if err = pageLimit(o.Limit); err != nil {
		return nil, err
	}
	for _, v := range o.Visibility {
		if !v.Valid() {
			return nil, invalid("visibility must be public, unlisted or private")
		}
	}
	return w.gateway.List(ctx, o)
}
func (w *Workspace) Stat(ctx context.Context, p string) (*api.Object, error) {
	p, err := w.object(p, api.ScopeRO)
	if err != nil {
		return nil, err
	}
	return w.gateway.Stat(ctx, p)
}

func (w *Workspace) Move(ctx context.Context, from, to string, recursive bool) (*api.MoveResponse, error) {
	var err error
	if recursive {
		from, err = w.directory(from, api.ScopeRW, false)
		if err != nil {
			return nil, err
		}
		to, err = w.directory(to, api.ScopeRW, false)
		if err != nil {
			return nil, err
		}
		if from == "" || to == "" || strings.HasPrefix(to, from) {
			return nil, invalid("cannot move the root or move a directory into itself")
		}
	} else {
		from, err = w.object(from, api.ScopeRW)
		if err != nil {
			return nil, err
		}
		to, err = w.object(to, api.ScopeRW)
		if err != nil {
			return nil, err
		}
	}
	if from == to {
		return nil, invalid("source and destination must differ")
	}
	return w.gateway.Move(ctx, api.MoveRequest{From: from, To: to, Recursive: recursive, Overwrite: false})
}
func (w *Workspace) Delete(ctx context.Context, p string, recursive, purge bool, rev int64) (*api.DeleteResponse, error) {
	scope := api.ScopeRW
	if purge {
		scope = api.ScopeAdmin
	}
	var err error
	if recursive {
		p, err = w.directory(p, scope, false)
		if p == "" && err == nil {
			return nil, invalid("deleting the whole root is not allowed")
		}
		if rev != 0 {
			return nil, invalid("directory deletion does not support expected_revision")
		}
	} else {
		p, err = w.object(p, scope)
	}
	if err != nil {
		return nil, err
	}
	if err = revision(rev); err != nil {
		return nil, err
	}
	return w.gateway.Delete(ctx, p, DeleteOptions{Recursive: recursive, Purge: purge, IfRevision: rev})
}
func (w *Workspace) ListTrash(ctx context.Context, o TrashOptions) (*api.TrashList, error) {
	var err error
	o.Prefix, err = w.directory(o.Prefix, api.ScopeRW, true)
	if err != nil {
		return nil, err
	}
	if o.Path != "" {
		o.Path, err = w.object(o.Path, api.ScopeRW)
		if err != nil {
			return nil, err
		}
	}
	if err = pageLimit(o.Limit); err != nil {
		return nil, err
	}
	return w.gateway.ListTrash(ctx, o)
}

// trashEntry binds an opaque ID to an authorized original path before restore
// or purge. This matters if a future resolver narrows a broader upstream key.
func (w *Workspace) trashEntry(ctx context.Context, id, original string, scope api.Scope) (*api.TrashEntry, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	p, err := w.object(original, scope)
	if err != nil {
		return nil, err
	}
	o := TrashOptions{Path: p, Prefix: w.identity.Prefix, Limit: 1000}
	for {
		page, err := w.gateway.ListTrash(ctx, o)
		if err != nil {
			return nil, err
		}
		for _, e := range page.Entries {
			if e.ID == id && e.OriginalPath == p {
				return &e, nil
			}
		}
		if page.NextCursor == "" {
			return nil, invalid("trash_id does not match original_path in the accessible trash")
		}
		if page.NextCursor == o.Cursor {
			return nil, invalid("server returned a repeated cursor")
		}
		o.Cursor = page.NextCursor
	}
}
func (w *Workspace) Restore(ctx context.Context, id, original, to string) (*api.Object, error) {
	if _, err := w.trashEntry(ctx, id, original, api.ScopeRW); err != nil {
		return nil, err
	}
	if to != "" {
		var err error
		to, err = w.object(to, api.ScopeRW)
		if err != nil {
			return nil, err
		}
	}
	return w.gateway.Restore(ctx, id, api.RestoreRequest{Path: to, Overwrite: false})
}
func (w *Workspace) PurgeTrashEntry(ctx context.Context, id, original string) error {
	if _, err := w.trashEntry(ctx, id, original, api.ScopeAdmin); err != nil {
		return err
	}
	return w.gateway.PurgeTrashEntry(ctx, id)
}
func (w *Workspace) EmptyTrash(ctx context.Context, olderThan string) (*api.PurgeResponse, error) {
	if err := access.Require(w.identity, api.ScopeAdmin); err != nil {
		return nil, err
	}
	// The bulk endpoint has no path selector. Refuse narrowed identities.
	if w.identity.Prefix != "" {
		return nil, access.ErrForbidden
	}
	if olderThan != "" {
		d, err := api.ParseDuration(olderThan)
		if err != nil || d <= 0 {
			return nil, invalid("older_than must be a positive Vela duration such as 7d")
		}
	}
	return w.gateway.PurgeTrash(ctx, olderThan)
}
