# Tools and business workflows

Based on [Vela API v1](https://docs.xchunk.org/vela/docs/api-reference.html), [visibility](https://docs.xchunk.org/vela/docs/visibility.html), [signed links](https://docs.xchunk.org/vela/docs/signed-links.html), [trash](https://docs.xchunk.org/vela/docs/trash.html), and [directory policies](https://docs.xchunk.org/vela/docs/directories.html). MCP exposes business operations rather than HTTP methods. Text/attachment creation applies metadata in the same conditional upload and explicitly defaults to private. HTTP transfer preparation delegates raw byte exchange to the client's computing environment.

There are 31 configurable tools. The standard stdio set registers 22; HTTP registers 24 with transfers enabled. Only `search_files` was added for discovery; browsing and reading remain separate existing tools. All tools below support both transports except `download_file` (stdio only) and the three HTTP transfer tools. `upload_file` and an enabled `replace_file` accept inline bytes over HTTP, but local paths remain unavailable. Scope in the table is the minimum upstream Vela key authority, not a distinct OAuth scope; HTTP tokens use `vela` and inherit the entered key's actual rights/prefix.

Inputs use JSON Schema with unknown fields rejected. Results include both `structuredContent` and a JSON text representation in `content`. API failures set `isError=true` and expose `error.code`, `message`, `status`, and, where available, `request_id`, `details`, and recovery guidance. Schema errors are also tool failures handled by the SDK. Unknown/disabled tools produce protocol errors. Use tools actually present in discovery: a disabled transfer or search tool is not an available recovery route.

| Tool | Default | Key | Workflow and main arguments |
| --- | --- | --- | --- |
| `server_info` | on | ro | Diagnose effective scope/prefix, compatibility, or limits when needed; no required preflight |
| `list_directory` | on | ro | `prefix`, `recursive=false`, `visibility[]`, `limit=50`, `cursor`, `response_format=concise/detailed`; returns `objects`, `dirs`, `next_cursor` |
| `search_files` | on | ro | `query`, `prefix?`, `visibility[]`, `limit=20`, `cursor`; metadata matches in `files`, `scanned`, `complete`, `next_cursor` |
| `get_file_metadata` | on | ro | `path`; all metadata and `revision` |
| `read_file` | on | ro | `path`, `encoding=auto/utf-8/base64`, `max_bytes`, `response_format=concise/detailed`; content, revision and preview status |
| `download_file` | on, stdio only | ro | `path`, `local_path`; save a complete verified file in download roots without overwriting |
| `create_text_file` | on | rw | `path`, `text`, `metadata`; create a new file, including empty text |
| `upload_file` | on | rw | `path`, exactly one of `local_path`/`content_base64`, `metadata`; add a new file; local paths work only in stdio |
| `replace_file` | **off** | rw | `path`, `expected_revision`, one of `text`/`local_path`/`content_base64`, `metadata`; replace contents |
| `update_file_metadata` | on | rw | `path`, `patch`, `expected_revision?`; edit/reset selected fields |
| `set_file_visibility` | on | rw | `path`, `visibility`, `expected_revision?`; private/unlisted/public |
| `move_file` | on | rw | `from`, `to`; move/rename without overwriting |
| `move_directory` | on | rw | `from`, `to` directory prefixes; atomic move of a subtree and its policies |
| `trash_file` | on | rw | `path`, `expected_revision?`; recoverable deletion of one file |
| `trash_directory` | **off** | rw | `path` directory prefix; recoverable bulk deletion; root is forbidden |
| `list_trash` | on | rw | `prefix?`, `path?`, `limit`, `cursor`; discover original paths, IDs, and purge deadlines |
| `restore_file` | on | rw | `trash_id`, `original_path`, `to_path?`; restore the same object ID without overwriting |
| `create_share_link` | on | rw | `path`, `ttl?` **or** `expires_at?`, `note?`; temporary signed URL |
| `list_share_links` | on | rw | `path?`, `include_expired=false`, `include_revoked=false`, `limit`, `offset`; links and `next_offset` |
| `revoke_share_link` | on | rw | `link_id`, `path?`; revoke one link; path is required for a scoped identity |
| `inspect_directory` | on | ro | `prefix`; effective settings, inheritance sources, available URL |
| `list_directory_policies` | on | ro | `prefix`, `recursive=true`, `orphaned=false`, `limit`, `cursor`; explicit rules |
| `update_directory_policy` | on | rw | `prefix`, `patch`, `expected_revision?`; edit/create one non-root rule |
| `reset_directory_policy` | on | rw | `prefix`, `expected_revision?`; remove a non-root rule, restoring inheritance |
| `update_root_directory_policy` | **off** | unrestricted admin | `patch`, `expected_revision?`; change the store's root policy |
| `purge_file` | **off** | admin | `path`, `expected_revision?`; permanently delete one live file |
| `purge_trash_entry` | **off** | admin | `trash_id`, `original_path`; permanently delete one trash entry |
| `empty_trash` | **off** | unrestricted admin | `older_than?`; purge old entries or **all** trash if omitted |
| `prepare_file_upload` | on, HTTP only | rw | `path`, `size`, `sha256`, `metadata`; single-use raw PUT instructions, create-only |
| `prepare_file_download` | on, HTTP only | ro | `path`; expiring original-byte URL, filename, object metadata, checksum and size |
| `get_file_transfer_status` | on, HTTP only | owner | `transfer_id`; inspect your transfer, verified receipt and uncertain-result recovery |

`download_file` requires configured stdio roots. HTTP always omits it, even when its switch is true. HTTP transfer tools require `transfers.enabled=true` and their own tool switches. Disabling the broker removes its routes as well. `upload_file` works through base64 without roots; HTTP upload/replacement schemas omit `local_path` entirely. The standard set permits publishing and recoverable single-file deletion; use an `ro` key and disable unwanted tools for a read-only deployment. Switches remove discovery/execution without expanding key permissions.

## Find, browse, and read

For a known text path, call `read_file` directly. To browse a directory, use `list_directory`; to locate a file by its name or metadata, use `search_files`. Neither needs an initial `server_info` or metadata call. `get_file_metadata` is useful when metadata itself is the task.

`list_directory` defaults to `response_format="concise"`. Its `objects` contain `path`, `size`, `content_type`, `visibility`, and `revision`, plus nonempty `title` and `tags`. IDs, hashes, descriptions, timestamps, and URLs are omitted. `dirs`, `prefix`, and `next_cursor` retain their usual meaning. `response_format="detailed"` restores complete Vela objects.

`search_files` accepts a literal case-insensitive substring of 1–256 characters, matched against path, title, description, and individual tags. It recursively scans metadata under the authorized prefix; it does not download or search file contents. Results are compact records in path order. Defaults are 20 matches, maximum 100. Each invocation scans at most five REST pages/500 objects; sparse matches can therefore require more backend requests than browsing. Narrow the prefix when possible.

Search returns `scanned`, `complete`, and `next_cursor`. A scan can return zero matches with `complete=false`: this is an incomplete result, not evidence that the file does not exist. Resume with the same query, prefix, and visibility only when more matches are needed. Search cursors are bound to those filters and the caller; they are not snapshots of a changing store. Stop when the user's task is satisfied.

A concise `read_file` result has this shape:

```json
{
  "path": "notes/hello.txt",
  "revision": 7,
  "size": 6,
  "encoding": "utf-8",
  "content": "Hello\n",
  "bytes_returned": 6,
  "truncated": false
}
```

Detailed reads add the full metadata as `object`. Automatic encoding accepts readable text and rejects binary content; use an available download tool for original binary files. Explicit `encoding="base64"` is for small byte inspection, while `encoding="utf-8"` requests text interpretation explicitly. A preview may be shortened further to fit the serialized HTTP budget. UTF-8 characters are not cut in half, and `truncated=true` always means the content is only a prefix. Download the complete original before replacing a truncated file.

Reuse a returned revision for a guarded metadata edit or replacement. A stale revision means the file changed: inspect its current state and reconsider the edit rather than obtaining a new revision and blindly replaying the old write.

## HTTP binary workflows

Use `prepare_file_upload` for files attached to a cloud conversation or generated in the client's own environment. Compute the raw file size and SHA-256 there, prepare the logical Vela destination with metadata, and stream those exact bytes to the returned URL using its method/headers. Preparation itself does not upload content. The capability cannot change destination, metadata, size or hash, and cannot overwrite an existing file. `replace_file` stays separately enabled with a required revision; the transfer broker has no replacement mode.

Use `prepare_file_download` for any file type without text extraction or base64 in the conversation. A read-only key is sufficient, including for private files. The URL can be downloaded in the client's environment, opened in a browser, or returned to the user. It expires, supports Range/HEAD, and stops working when the owner's OAuth grant is revoked. The server does not change visibility or create a Vela signed link for this workflow. Host network permissions and native attachment support still apply.

`get_file_transfer_status` is restricted to the authenticated grant owner. States include `prepared`, `uploading`, `succeeded`, `failed`, `uncertain` and `expired`. Successful uploads include a persisted Vela receipt; a successful PUT receipt needs no extra status call. An uncertain upload may include matching destination metadata, but matching bytes alone do not prove that the original request created the object. Inspect before retrying. Download success records that verified bytes were available for serving, not that a remote host saved the complete file. See the [complete byte-transfer contract](http-file-transfer.md).

## Metadata

Creation/upload/replacement `metadata` accepts:

```json
{
  "visibility": "private",
  "title": "Report",
  "description": "For review",
  "tags": ["report", "review"],
  "content_type": "application/pdf",
  "disposition": "attachment",
  "expires_at": "7d"
}
```

`expires_at` accepts a future RFC3339 timestamp or a Vela duration (`90m`, `2h`, `7d`, `2w`); `none` clears expiry on replacement. `tags:[]` and empty title/description clear those values. Tags are normalized to lowercase. Creation explicitly chooses private if visibility is omitted. Replacement preserves unspecified metadata; binary MIME is detected again, while UTF-8 text sources default to `text/plain; charset=utf-8`.

`update_file_metadata.patch` is a JSON merge patch over the same seven fields. Omitted fields stay unchanged; `null` resets a field to Vela's default. For example:

```json
{
  "path": "docs/report.pdf",
  "expected_revision": 3,
  "patch": {
    "title": "Final report",
    "tags": ["final"],
    "expires_at": null,
    "visibility": "private"
  }
}
```

A patch expiry accepts only an RFC3339 timestamp or null. `content_type:null` redetects MIME. `visibility:null` selects **Vela's server default**, which may expose content; use explicit `private` for confidentiality. Change paths through move. `expected_revision` maps to `If-Match` and protects concurrent edits; replacement requires it.

## Access, links, and trash

`public` files appear in the index and are downloadable without a key. `unlisted` files are hidden from listings but downloadable by anyone with the URL. `private` requires a key or signed link. `hidden`, `noindex`, and `index=off` control presentation/crawlers and do not replace private visibility.

Signed links are bound to object IDs and survive moves. `create_share_link` returns a URL, expiry, and link ID; TTL above the server maximum fails. A signed URL is an access credential: it is returned to the user but never written to MCP diagnostic logs. Revocation is immediate. Revoking a link to a trashed file without a current path is supported only for an unrestricted identity in this server, preventing opaque IDs from bypassing local prefix policy.

Trash has separate IDs. `restore_file` and `purge_trash_entry` verify the ID against an accessible `original_path`; obtain both from `list_trash`. Restore preserves the object ID and clears a past expiry. Restore and move never overwrite existing objects. Vela controls retention, typically 14 days; `purge_at` is authoritative.

This server disallows `empty_trash` for prefix-restricted identities even if the upstream API supports scoped purges. The bulk API has no separate prefix selector, so the application cannot safely express narrower OAuth authority through a broader credential. Individual trash purges work with a verified original path.

## Pagination and limits

Objects, trash, and directory policies return one page (default 50, maximum 1000). Continue `next_cursor` with unchanged filters only if needed. Root/ancestor listings with prefix-scoped keys are narrowed to their permitted subtree. Directory inspection requires an accessible prefix. HTTP caps the complete serialized tool result at 128,000 bytes by default, including text and structured forms; for an oversized listing, use concise output or a smaller `limit`. This budget is separate from the 50 MiB MCP JSON request limit.

Vela's links endpoint is unpaginated. MCP limits output with `limit` (default 50, maximum 1000), returning `total` and `next_offset` (null at the end). Offsets apply to a fresh upstream list: concurrent changes can shift entries; deduplicate by ID. The public client still fetches the entire filtered upstream response.

Conversation reads default to at most 32 KiB raw content (`files.max_read_bytes`). Raising that setting or requesting a large `max_bytes` still respects the HTTP output budget; use `prepare_file_download` for complete large or binary files. Complete reads/downloads verify SHA-256 against preliminary metadata. A truncated read cannot verify the full-file hash; metadata is a snapshot before reading. If a successful mutation's receipt exceeds the HTTP budget, the server returns `completed=true`, `receipt_truncated=true`, the tool name, and recovery guidance instead of turning the completed operation into an error. Inspect the resulting state; do not repeat the mutation just to obtain its receipt.

An interrupted mutation may already have committed: inspect state/revision before retrying. POST move/share/restore are not automatically retried. HTTP uploads are single-attempt: equal existing bytes cannot prove that a lost request applied its metadata. Safe reads retain configured retries; conditional stdio uploads retain the public client's recovery behavior.

All public REST business features are covered, except the separate health probe and low-level full-replacement policy PUT (the same policy values can be set with patch). API-key administration, signing-key rotation, janitor, backups, and storage migration are local `velad` operations outside REST and have no MCP tools.
