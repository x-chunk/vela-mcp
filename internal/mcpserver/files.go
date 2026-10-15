package mcpserver

import (
	"context"

	"go.xchunk.org/vela-mcp/internal/app"
	"go.xchunk.org/vela/api"
)

type noArgs struct{}
type pathArgs struct {
	Path             string `json:"path"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
}
type listArgs struct {
	Prefix         string           `json:"prefix,omitempty"`
	Recursive      bool             `json:"recursive,omitempty"`
	Visibility     []api.Visibility `json:"visibility,omitempty"`
	Limit          int              `json:"limit,omitempty"`
	Cursor         string           `json:"cursor,omitempty"`
	ResponseFormat string           `json:"response_format,omitempty"`
}
type searchArgs struct {
	Query      string           `json:"query"`
	Prefix     string           `json:"prefix,omitempty"`
	Visibility []api.Visibility `json:"visibility,omitempty"`
	Limit      int              `json:"limit,omitempty"`
	Cursor     string           `json:"cursor,omitempty"`
}
type readArgs struct {
	Path           string `json:"path"`
	Encoding       string `json:"encoding,omitempty"`
	MaxBytes       int64  `json:"max_bytes,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}
type downloadArgs struct {
	Path      string `json:"path"`
	LocalPath string `json:"local_path"`
}
type textArgs struct {
	Path     string       `json:"path"`
	Text     string       `json:"text"`
	Metadata app.Metadata `json:"metadata,omitempty"`
}
type uploadArgs struct {
	Path             string       `json:"path"`
	LocalPath        string       `json:"local_path,omitempty"`
	ContentBase64    *string      `json:"content_base64,omitempty"`
	Text             *string      `json:"text,omitempty"`
	Metadata         app.Metadata `json:"metadata,omitempty"`
	ExpectedRevision int64        `json:"expected_revision,omitempty"`
}
type patchArgs struct {
	Path             string          `json:"path"`
	Patch            api.ObjectPatch `json:"patch"`
	ExpectedRevision int64           `json:"expected_revision,omitempty"`
}
type visibilityArgs struct {
	Path             string         `json:"path"`
	Visibility       api.Visibility `json:"visibility"`
	ExpectedRevision int64          `json:"expected_revision,omitempty"`
}

func (r *registrar) files() {
	register(r, "server_info", "Diagnose server limits or the caller's allowed scope/prefix when needed. Ordinary file operations do not need this preflight. Never returns a bearer secret.", object(schema{}), func(ctx context.Context, w *app.Workspace, _ noArgs) (any, error) { return w.Info(ctx) })
	visibility := array(enum("Visibility filter.", "public", "unlisted", "private"), "Only these visibility levels; omit for all.")
	listDescription := "Browse a directory: immediate children by default, recursive files if requested. Returns 50 compact records with paths and revisions; use detailed only for full metadata. Continue next_cursor only if more results are needed."
	if r.toolEnabled("search_files") {
		listDescription += " Use search_files to find a file by name or metadata."
	}
	register(r, "list_directory", listDescription, pagination(schema{"prefix": dirPath(), "recursive": boolean("Return descendant files instead of immediate children. Default false."), "visibility": visibility, "response_format": responseFormat()}), func(ctx context.Context, w *app.Workspace, a listArgs) (any, error) {
		if a.Limit == 0 {
			a.Limit = 50
		}
		page, err := w.List(ctx, app.ListOptions{Prefix: a.Prefix, Recursive: a.Recursive, Visibility: a.Visibility, Limit: a.Limit, Cursor: a.Cursor})
		if err != nil {
			return nil, err
		}
		return listingResult(page, a.ResponseFormat), nil
	})
	register(r, "search_files", "Find files by a literal case-insensitive substring in path, title, description or tags. Searches metadata only, never file contents. Returns up to 20 compact matches in path order. A call scans at most 5 metadata pages/500 files; empty results prove absence only when complete=true. Resume next_cursor with the same query/filters only if needed.", object(schema{"query": schema{"type": "string", "minLength": 1, "maxLength": 256, "description": "Literal substring, e.g. report or a tag. No glob, regex, semantic or file-content search."}, "prefix": dirPath(), "visibility": visibility, "limit": integer("Maximum matches, default 20. A bounded scan may return fewer with a continuation cursor.", 1, 100), "cursor": str("Opaque next_cursor from search_files with the same query, prefix and visibility.")}, "query"), func(ctx context.Context, w *app.Workspace, a searchArgs) (any, error) {
		page, err := w.Search(ctx, app.SearchOptions{Query: a.Query, Prefix: a.Prefix, Visibility: a.Visibility, Limit: a.Limit, Cursor: a.Cursor})
		if err != nil {
			return nil, err
		}
		return searchResult(page), nil
	})
	register(r, "get_file_metadata", "Inspect one file's complete metadata, SHA256, visibility and revision without downloading its content.", object(schema{"path": filePath()}, "path"), func(ctx context.Context, w *app.Workspace, a pathArgs) (any, error) { return w.Stat(ctx, a.Path) })
	register(r, "read_file", "Read a known file's text directly; no metadata preflight needed. Default returns content, revision and preview status without the full metadata object. Auto refuses binary previews; download original bytes instead. Explicit base64 is for small byte inspection. A truncated result is only a prefix: download the whole file before replacing it.", object(schema{"path": filePath(), "encoding": enum("Default auto: readable text only. utf-8 forces text interpretation; base64 explicitly requests binary bytes.", "auto", "utf-8", "base64"), "max_bytes": integer("Maximum raw preview bytes, capped by files.max_read_bytes and the serialized response budget. May return fewer bytes with truncated=true.", 1, 0), "response_format": responseFormat()}, "path"), func(ctx context.Context, w *app.Workspace, a readArgs) (any, error) {
		v, err := w.Read(ctx, a.Path, a.Encoding, a.MaxBytes)
		if err != nil {
			return nil, err
		}
		if (a.Encoding == "" || a.Encoding == "auto") && (v.Encoding == "base64" || binaryContentType(v.Object.ContentType)) {
			help := "use a configured file-download tool for original bytes"
			if r.transferService != nil && r.toolEnabled("prepare_file_download") {
				help = "use prepare_file_download to get the original file"
			} else if r.service.Files != nil && r.toolEnabled("download_file") {
				help = "use download_file to save the original file"
			}
			return nil, &app.Invalid{Message: "this file is binary; " + help + ", or explicitly request encoding=base64 for small byte inspection"}
		}
		return r.readResult(v, a.ResponseFormat)
	})
	register(r, "download_file", "Save a complete file to an absolute path inside configured download_roots. Parent directory must exist; target must not exist. Verify SHA256 and remove incomplete files.", object(schema{"path": filePath(), "local_path": str("Absolute destination path visible to this MCP process/container.")}, "path", "local_path"), func(ctx context.Context, w *app.Workspace, a downloadArgs) (any, error) {
		return w.Download(ctx, a.Path, a.LocalPath)
	})
	register(r, "create_text_file", "Create a NEW file from generated UTF-8 text, applying all metadata in the same upload. Default visibility private. Existing paths produce already_exists; never overwrite.", object(schema{"path": filePath(), "text": str("UTF-8 contents, including an empty string for an empty file."), "metadata": metadata()}, "path", "text"), func(ctx context.Context, w *app.Workspace, a textArgs) (any, error) {
		return w.Upload(ctx, a.Path, app.Source{Text: &a.Text}, a.Metadata, false, 0)
	})
	sources := schema{"path": filePath(), "content_base64": str("Small standard padded base64 bytes, without a data URI. Do not manually reconstruct large attachments as base64."), "metadata": metadata()}
	sourceChoices := []any{schema{"required": []string{"content_base64"}}}
	uploadDescription := "Create a new file from small base64 bytes with metadata in one call; private by default, never overwrites. Use create_text_file for generated text."
	if r.transferService != nil && r.toolEnabled("prepare_file_upload") {
		uploadDescription += " For chat attachments or large files use prepare_file_upload and send original bytes."
	}
	if r.service.Files != nil {
		sources["local_path"] = str("Absolute regular file inside configured stdio upload_roots. A chat attachment's path belongs to the client's environment and may not be accessible here.")
		sourceChoices = append(sourceChoices, schema{"required": []string{"local_path"}})
		uploadDescription = "Create a new file from a mounted local_path OR small content_base64 with metadata in one call. Private by default, never overwrites. local_path must be visible to this stdio process."
	}
	s := object(sources, "path")
	s["oneOf"] = sourceChoices
	register(r, "upload_file", uploadDescription, s, func(ctx context.Context, w *app.Workspace, a uploadArgs) (any, error) {
		return w.Upload(ctx, a.Path, app.Source{LocalPath: a.LocalPath, Base64: a.ContentBase64}, a.Metadata, false, 0)
	})
	replace := object(schema{"path": filePath(), "text": str("Complete replacement UTF-8 contents, never a truncated preview."), "content_base64": sources["content_base64"], "metadata": metadata(), "expected_revision": rev()}, "path", "expected_revision")
	if r.service.Files != nil {
		replace["properties"].(schema)["local_path"] = sources["local_path"]
	}
	replace["oneOf"] = append([]any{schema{"required": []string{"text"}}}, sourceChoices...)
	register(r, "replace_file", "Replace existing content ONLY at expected_revision. Requires explicit config enablement. Keeps object ID, signed links and omitted metadata; content type is redetected for binary unless supplied.", replace, func(ctx context.Context, w *app.Workspace, a uploadArgs) (any, error) {
		return w.Upload(ctx, a.Path, app.Source{LocalPath: a.LocalPath, Base64: a.ContentBase64, Text: a.Text}, a.Metadata, true, a.ExpectedRevision)
	})
	register(r, "update_file_metadata", "Edit selected file metadata with JSON merge patch. Omitted fields stay unchanged; null resets defaults (visibility:null resets to Vela's default, which can expose content). expires_at:null clears expiry; tags:[] clears tags. Use expected_revision to protect concurrent edits.", object(schema{"path": filePath(), "patch": metadataPatch(), "expected_revision": rev()}, "path", "patch"), func(ctx context.Context, w *app.Workspace, a patchArgs) (any, error) {
		return w.Patch(ctx, a.Path, a.Patch, a.ExpectedRevision)
	})
	register(r, "set_file_visibility", "Set private (key/signed link required), unlisted (anyone with URL), or public (listed and downloadable). Directory index settings are separate. Publishing exposes the contents.", object(schema{"path": filePath(), "visibility": enum("Desired visibility.", "private", "unlisted", "public"), "expected_revision": rev()}, "path", "visibility"), func(ctx context.Context, w *app.Workspace, a visibilityArgs) (any, error) {
		return w.Visibility(ctx, a.Path, a.Visibility, a.ExpectedRevision)
	})
}
