package mcpserver

import (
	"context"

	"go.xchunk.org/vela-mcp/internal/app"
)

type moveArgs struct {
	From string `json:"from"`
	To   string `json:"to"`
}
type trashListArgs struct {
	Prefix string `json:"prefix,omitempty"`
	Path   string `json:"path,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}
type restoreArgs struct {
	TrashID      string `json:"trash_id"`
	OriginalPath string `json:"original_path"`
	ToPath       string `json:"to_path,omitempty"`
}
type emptyArgs struct {
	OlderThan string `json:"older_than,omitempty"`
}

func done(err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}
func (r *registrar) organization() {
	register(r, "move_file", "Rename or move one file without changing contents or overwriting a destination. Signed links and object ID survive; its direct public URL changes.", object(schema{"from": filePath(), "to": filePath()}, "from", "to"), func(ctx context.Context, w *app.Workspace, a moveArgs) (any, error) {
		return w.Move(ctx, a.From, a.To, false)
	})
	register(r, "move_directory", "Move all files and directory policies under a prefix in one atomic operation. A destination conflict aborts the whole move. Cannot move the root or move a directory into itself.", object(schema{"from": dirPath(), "to": dirPath()}, "from", "to"), func(ctx context.Context, w *app.Workspace, a moveArgs) (any, error) {
		return w.Move(ctx, a.From, a.To, true)
	})
	register(r, "trash_file", "Delete one file into recoverable trash. Returns trash_ids for recovery; Vela decides retention. Reuse a known revision as expected_revision to guard concurrent changes.", object(schema{"path": filePath(), "expected_revision": rev()}, "path"), func(ctx context.Context, w *app.Workspace, a pathArgs) (any, error) {
		return w.Delete(ctx, a.Path, false, false, a.ExpectedRevision)
	})
	register(r, "trash_directory", "Move an entire subtree into recoverable trash. Disabled by default because this is a bulk deletion. Root deletion is rejected; directory policies remain.", object(schema{"path": dirPath()}, "path"), func(ctx context.Context, w *app.Workspace, a pathArgs) (any, error) {
		return w.Delete(ctx, a.Path, true, false, 0)
	})
	register(r, "list_trash", "Find deleted files to restore or inspect, newest first. Prefer an exact path or narrow prefix; returns original_path, trash ID and purge_at. Requires rw. Continue with next_cursor only if needed.", pagination(schema{"prefix": dirPath(), "path": filePath()}), func(ctx context.Context, w *app.Workspace, a trashListArgs) (any, error) {
		if a.Limit == 0 {
			a.Limit = 50
		}
		return w.ListTrash(ctx, app.TrashOptions{Prefix: a.Prefix, Path: a.Path, Limit: a.Limit, Cursor: a.Cursor})
	})
	register(r, "restore_file", "Restore a deleted file using trash_id and original_path from list_trash. Omit to_path to restore in place. Both paths must be accessible; never overwrites. Object ID is preserved and a past expiry is cleared.", object(schema{"trash_id": id(), "original_path": filePath(), "to_path": filePath()}, "trash_id", "original_path"), func(ctx context.Context, w *app.Workspace, a restoreArgs) (any, error) {
		return w.Restore(ctx, a.TrashID, a.OriginalPath, a.ToPath)
	})
	register(r, "purge_file", "Permanently delete ONE live file and its links, bypassing trash. Irreversible; requires admin and explicit config enablement. No recursive purge.", object(schema{"path": filePath(), "expected_revision": rev()}, "path"), func(ctx context.Context, w *app.Workspace, a pathArgs) (any, error) {
		return w.Delete(ctx, a.Path, false, true, a.ExpectedRevision)
	})
	register(r, "purge_trash_entry", "Permanently remove one trash entry and its links. Irreversible; requires admin and explicit config enablement. Bind trash_id to original_path from list_trash.", object(schema{"trash_id": id(), "original_path": filePath()}, "trash_id", "original_path"), func(ctx context.Context, w *app.Workspace, a restoreArgs) (any, error) {
		return done(w.PurgeTrashEntry(ctx, a.TrashID, a.OriginalPath))
	})
	register(r, "empty_trash", "Permanently purge trash entries older than older_than, or ALL trash if omitted. Irreversible; requires an unrestricted admin identity and explicit config enablement.", object(schema{"older_than": str("Positive Vela duration such as 7d; omit to empty the whole trash.")}), func(ctx context.Context, w *app.Workspace, a emptyArgs) (any, error) {
		return w.EmptyTrash(ctx, a.OlderThan)
	})
}
