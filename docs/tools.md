# Tools and business workflows

Based on [Vela API v1](https://docs.xchunk.org/vela/docs/api-reference.html), [visibility](https://docs.xchunk.org/vela/docs/visibility.html), [signed links](https://docs.xchunk.org/vela/docs/signed-links.html), [trash](https://docs.xchunk.org/vela/docs/trash.html), and [directory policies](https://docs.xchunk.org/vela/docs/directories.html). MCP exposes user operations rather than HTTP methods. For example, text/attachment creation uses a conditional streaming upload, applies metadata in the same request, computes SHA-256, and explicitly defaults to private.

Inputs use JSON Schema with unknown fields rejected. Results include both `structuredContent` and a JSON text representation in `content`. API failures set `isError=true` and expose `error.code`, `message`, `status`, and, where available, `request_id` and `details`. Schema errors are also tool failures handled by the SDK. Unknown/disabled tools produce protocol errors.

| Tool | Default | Key | Workflow and main arguments |
| --- | --- | --- | --- |
| `server_info` | on | ro | Server/API compatibility, effective scope/prefix, and limits; no arguments |
| `list_directory` | on | ro | `prefix`, `recursive=false`, `visibility[]`, `limit`, `cursor`; returns `objects`, `dirs`, `next_cursor` |
| `get_file_metadata` | on | ro | `path`; all metadata and `revision` |
| `read_file` | on | ro | `path`, `encoding=auto/utf-8/base64`, `max_bytes`; bounded `content`, `truncated`, `bytes_returned` |
| `download_file` | on | ro | `path`, `local_path`; save a complete verified file in download roots without overwriting |
| `create_text_file` | on | rw | `path`, `text`, `metadata`; create a new file, including empty text |
| `upload_file` | on | rw | `path`, exactly one of `local_path`/`content_base64`, `metadata`; add a new file |
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

`download_file` is registered by default, but local saving requires configured roots. `upload_file` works through base64 without roots. The standard set permits publishing and recoverable single-file deletion: these are intended workflows, while MCP annotations describe their mutation/destructive behavior. Use an `ro` key and disable unwanted tools for a read-only deployment.

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

Signed links are bound to object IDs and survive moves. `create_share_link` returns a URL, expiry, and link ID; TTL above the server maximum fails. A signed URL is an access credential: it is returned to the user but never written to MCP diagnostic logs. Revocation is immediate. Revoking a link to a trashed file without a current path is supported only for an unrestricted identity in this prototype, preventing opaque IDs from bypassing local prefix policy.

Trash has separate IDs. `restore_file` and `purge_trash_entry` verify the ID against an accessible `original_path`; obtain both from `list_trash`. Restore preserves the object ID and clears a past expiry. Restore and move never overwrite existing objects. Vela controls retention, typically 14 days; `purge_at` is authoritative.

This prototype disallows `empty_trash` for prefix-restricted identities even if the upstream API supports scoped purges. The bulk API has no separate prefix selector, so the application cannot safely express narrower OAuth authority through a broader credential. Individual trash purges work with a verified original path.

## Pagination and limits

Objects, trash, and directory policies return one page (default 200, maximum 1000). Keep filters unchanged and pass `next_cursor` into the next call. Root/ancestor listings with prefix-scoped keys are narrowed to their permitted subtree. Directory inspection requires an accessible prefix.

Vela's links endpoint is unpaginated. MCP limits output with `limit` (default 200, maximum 1000), returning `total` and `next_offset` (null at the end). Offsets apply to a fresh upstream list: concurrent changes can shift entries; deduplicate by ID. The public client still fetches the entire filtered upstream response.

Complete reads/downloads verify SHA-256 against preliminary metadata. A truncated read cannot verify the full-file hash; metadata is a snapshot before reading, and `truncated=true` means the returned content is only a prefix. An interrupted mutation may already have committed: inspect state/revision before retrying. POST move/share/restore are not automatically retried. Conditional uploads and requests classified as retryable by the upstream client use bounded retries.

All public REST business features are covered, except the separate health probe and low-level full-replacement policy PUT (the same policy values can be set with patch). API-key administration, signing-key rotation, janitor, backups, and storage migration are local `velad` operations outside REST and have no MCP tools.
