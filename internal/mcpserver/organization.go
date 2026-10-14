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
	register(r, "move_file", "Rename or move one file without overwriting. Signed links and object ID survive; direct public URL changes.", object(schema{"from": filePath(), "to": filePath()}, "from", "to"), func(ctx context.Context, w *app.Workspace, a moveArgs) (any, error) {
		return w.Move(ctx, a.From, a.To, false)
	})
	register(r, "move_directory", "Atomically move all files and directory policies under a prefix. No overwrite; any conflict aborts the whole move. Cannot move the root or move a directory into itself.", object(schema{"from": dirPath(), "to": dirPath()}, "from", "to"), func(ctx context.Context, w *app.Workspace, a moveArgs) (any, error) {
		return w.Move(ctx, a.From, a.To, true)
	})
	register(r, "trash_file", "Move one file into recoverable trash. Returns trash_ids; retention/purge_at is decided by Vela. Optionally guard with expected_revision.", object(schema{"path": filePath(), "expected_revision": rev()}, "path"), func(ctx context.Context, w *app.Workspace, a pathArgs) (any, error) {
		return w.Delete(ctx, a.Path, false, false, a.ExpectedRevision)
	})
	register(r, "trash_directory", "Move an entire subtree into recoverable trash. Disabled by default because this is a bulk deletion. Root deletion is rejected; directory policies remain.", object(schema{"path": dirPath()}, "path"), func(ctx context.Context, w *app.Workspace, a pathArgs) (any, error) {
		return w.Delete(ctx, a.Path, true, false, 0)
	})
	register(r, "list_trash", "Find deleted files, their original_path, trash ID and purge_at, newest first. Requires rw even though it is a read. Follow next_cursor.", pagination(schema{"prefix": dirPath(), "path": filePath()}), func(ctx context.Context, w *app.Workspace, a trashListArgs) (any, error) {
		return w.ListTrash(ctx, app.TrashOptions{Prefix: a.Prefix, Path: a.Path, Limit: a.Limit, Cursor: a.Cursor})
	})
	register(r, "restore_file", "Restore a trash entry to original_path or an optional to_path, preserving its object ID. Both original and target paths must be accessible. No overwrite; a past expiry is cleared by Vela.", object(schema{"trash_id": id(), "original_path": filePath(), "to_path": filePath()}, "trash_id", "original_path"), func(ctx context.Context, w *app.Workspace, a restoreArgs) (any, error) {
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
