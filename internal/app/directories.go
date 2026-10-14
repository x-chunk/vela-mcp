package app

import (
	"context"

	"go.xchunk.org/vela/api"
)

func (w *Workspace) InspectDirectory(ctx context.Context, p string) (*api.EffectiveDir, error) {
	p, err := w.directory(p, api.ScopeRO, false)
	if err != nil {
		return nil, err
	}
	return w.gateway.ShowDir(ctx, p)
}
func (w *Workspace) DirectoryPolicies(ctx context.Context, o DirListOptions) (*api.DirPolicyList, error) {
	var err error
	o.Prefix, err = w.directory(o.Prefix, api.ScopeRO, true)
	if err != nil {
		return nil, err
	}
	if err = pageLimit(o.Limit); err != nil {
		return nil, err
	}
	return w.gateway.ListDirPolicies(ctx, o)
}
func (w *Workspace) UpdateDirectory(ctx context.Context, p string, patch api.DirPolicyPatch, rev int64, root bool) (*api.EffectiveDir, error) {
	scope := api.ScopeRW
	if root {
		scope = api.ScopeAdmin
	}
	p, err := w.directory(p, scope, false)
	if err != nil {
		return nil, err
	}
	if root && p != "" {
		return nil, invalid("root policy operation requires the root directory")
	}
	if !root && p == "" {
		return nil, invalid("use the separately enabled update_root_directory_policy tool for the root")
	}
	if err = revision(rev); err != nil {
		return nil, err
	}
	if !patch.Index.Set && !patch.Hidden.Set && !patch.NoIndex.Set && !patch.Title.Set && !patch.Description.Set {
		return nil, invalid("patch must contain at least one directory policy field")
	}
	if patch.Index.HasValue() && !patch.Index.Value.Valid() || patch.NoIndex.HasValue() && !patch.NoIndex.Value.Valid() {
		return nil, invalid("index/noindex must be on, off, inherit or null")
	}
	if patch.Title.HasValue() {
		if err = api.ValidateTitle(patch.Title.Value); err != nil {
			return nil, invalid("%s", err)
		}
	}
	if patch.Description.HasValue() {
		if err = api.ValidateDescription(patch.Description.Value); err != nil {
			return nil, invalid("%s", err)
		}
	}
	return w.gateway.PatchDir(ctx, p, patch, rev)
}
func (w *Workspace) ResetDirectory(ctx context.Context, p string, rev int64) error {
	p, err := w.directory(p, api.ScopeRW, false)
	if err != nil {
		return err
	}
	if p == "" {
		return invalid("removing the root policy is not exposed")
	}
	if err = revision(rev); err != nil {
		return err
	}
	return w.gateway.DeleteDir(ctx, p, rev)
}

// Directory policies control listing and crawler hints, not content access.
// File confidentiality is controlled by object visibility and signed links.
