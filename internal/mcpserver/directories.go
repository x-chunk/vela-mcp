package mcpserver

import (
	"context"

	"go.xchunk.org/vela-mcp/internal/app"
	"go.xchunk.org/vela/api"
)

type directoryArgs struct {
	Prefix           string `json:"prefix,omitempty"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
}
type directoryListArgs struct {
	Prefix    string `json:"prefix,omitempty"`
	Recursive *bool  `json:"recursive,omitempty"`
	Orphaned  bool   `json:"orphaned,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
}
type directoryPatchArgs struct {
	Prefix           string             `json:"prefix,omitempty"`
	Patch            api.DirPolicyPatch `json:"patch"`
	ExpectedRevision int64              `json:"expected_revision,omitempty"`
}

func (r *registrar) directories() {
	register(r, "inspect_directory", "Explain a directory's effective public listing and crawler policy, including inherited sources and revision. Use list_directory for its files. These policies do not protect file contents.", object(schema{"prefix": dirPath()}), func(ctx context.Context, w *app.Workspace, a directoryArgs) (any, error) {
		return w.InspectDirectory(ctx, a.Prefix)
	})
	register(r, "list_directory_policies", "Find explicitly stored directory rules under a prefix, recursively by default. Use inspect_directory for effective inherited policy. orphaned=true selects rules without live public content below. Continue only if needed.", pagination(schema{"prefix": dirPath(), "recursive": boolean("Default true; false returns own and immediate child rules."), "orphaned": boolean("Select policies with no live public content below.")}), func(ctx context.Context, w *app.Workspace, a directoryListArgs) (any, error) {
		recursive := true
		if a.Recursive != nil {
			recursive = *a.Recursive
		}
		if a.Limit == 0 {
			a.Limit = 50
		}
		return w.DirectoryPolicies(ctx, app.DirListOptions{Prefix: a.Prefix, Recursive: recursive, Orphaned: a.Orphaned, Limit: a.Limit, Cursor: a.Cursor})
	})
	register(r, "update_directory_policy", "Set a non-root directory's listing, entrance, crawler hints or display metadata; creates a rule if absent. Omitted fields stay unchanged; null resets defaults. File visibility is changed with set_file_visibility.", object(schema{"prefix": dirPath(), "patch": directoryPatch(), "expected_revision": rev()}, "prefix", "patch"), func(ctx context.Context, w *app.Workspace, a directoryPatchArgs) (any, error) {
		return w.UpdateDirectory(ctx, a.Prefix, a.Patch, a.ExpectedRevision, false)
	})
	register(r, "reset_directory_policy", "Remove a non-root directory's explicit rule and return to inheritance. Files and descendant rules remain. Root removal is not exposed.", object(schema{"prefix": dirPath(), "expected_revision": rev()}, "prefix"), func(ctx context.Context, w *app.Workspace, a directoryArgs) (any, error) {
		return done(w.ResetDirectory(ctx, a.Prefix, a.ExpectedRevision))
	})
	register(r, "update_root_directory_policy", "Change the root policy for the whole Vela store (e.g. disable the public root index). Requires unrestricted admin and explicit config enablement. Root cannot be hidden.", object(schema{"patch": directoryPatch(), "expected_revision": rev()}, "patch"), func(ctx context.Context, w *app.Workspace, a directoryPatchArgs) (any, error) {
		return w.UpdateDirectory(ctx, "", a.Patch, a.ExpectedRevision, true)
	})
}
