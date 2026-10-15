// Package catalog is the single inventory of tool availability and risk defaults.
package catalog

type Tool struct {
	Name                                              string
	DefaultEnabled, ReadOnly, Destructive, Idempotent bool
	Purpose                                           string
}

// Tools returns a new slice so callers cannot mutate process-wide defaults.
func Tools() []Tool {
	return []Tool{
		{"server_info", true, true, false, true, "Server version, effective key and limits"},
		{"list_directory", true, true, false, true, "List immediate children or a recursive page"},
		{"search_files", true, true, false, true, "Find files by a bounded metadata search"},
		{"get_file_metadata", true, true, false, true, "Inspect metadata and revision"},
		{"read_file", true, true, false, true, "Read bounded text or base64 content"},
		{"download_file", true, false, false, false, "Save a verified file to an allowed local output directory"},
		{"create_text_file", true, false, false, false, "Create a new UTF-8 file with all metadata"},
		{"upload_file", true, false, false, false, "Add a new local file or base64 attachment with all metadata"},
		{"replace_file", false, false, true, false, "Replace content only at a required revision"},
		{"update_file_metadata", true, false, true, false, "Change selected metadata or reset fields"},
		{"set_file_visibility", true, false, true, false, "Set public, unlisted or private access"},
		{"move_file", true, false, true, false, "Rename or move one file without overwrite"},
		{"move_directory", true, false, true, false, "Atomically move a subtree and policies without overwrite"},
		{"trash_file", true, false, true, false, "Delete one file into recoverable trash"},
		{"trash_directory", false, false, true, false, "Delete a whole subtree into recoverable trash"},
		{"list_trash", true, true, false, true, "Find deleted files and restore IDs"},
		{"restore_file", true, false, false, false, "Restore a file without overwriting an existing path"},
		{"create_share_link", true, false, false, false, "Create an expiring signed URL"},
		{"list_share_links", true, true, false, true, "List working, expired or revoked signed URLs"},
		{"revoke_share_link", true, false, true, true, "Immediately revoke one signed URL"},
		{"inspect_directory", true, true, false, true, "Explain effective public index and crawler settings"},
		{"list_directory_policies", true, true, false, true, "List explicit rules including orphaned policies"},
		{"update_directory_policy", true, false, true, false, "Patch a non-root index, crawler hints and description"},
		{"reset_directory_policy", true, false, true, false, "Remove one non-root rule, returning to inheritance"},
		{"update_root_directory_policy", false, false, true, false, "Change the whole store's root index policy"},
		{"purge_file", false, false, true, false, "Permanently delete one live file"},
		{"purge_trash_entry", false, false, true, false, "Permanently delete one trashed file"},
		{"empty_trash", false, false, true, false, "Permanently purge the caller's trash, optionally by age"},
		{"prepare_file_upload", true, false, false, false, "Issue an HTTP upload capability bound to one file and its metadata"},
		{"prepare_file_download", true, false, false, false, "Issue a temporary HTTP download for any binary file"},
		{"get_file_transfer_status", true, true, false, true, "Inspect an owned HTTP transfer and its verified receipt"},
	}
}

// HTTPOnly tools require the optional byte-transfer broker and are never
// registered by a plain stdio server.
func HTTPOnly(name string) bool {
	return name == "prepare_file_upload" || name == "prepare_file_download" || name == "get_file_transfer_status"
}

func Defaults() map[string]bool {
	out := map[string]bool{}
	for _, t := range Tools() {
		out[t.Name] = t.DefaultEnabled
	}
	return out
}
func Validate(overrides map[string]bool) (string, bool) {
	known := Defaults()
	for name := range overrides {
		if _, ok := known[name]; !ok {
			return name, false
		}
	}
	return "", true
}
