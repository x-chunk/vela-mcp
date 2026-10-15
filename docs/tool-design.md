# Tool design: focused retrieval and fewer recovery calls

The `improving-tools` changes reduce irrelevant output and avoidable round trips
while retaining a small tool set. Claude's task success rate has not been measured.

## Research basis

Anthropic recommends distinct tools for common workflows, relevant return fields,
bounded output, and evaluations of calls, errors, and context usage. Its concise
versus detailed example supports an explicit verbosity choice.
[Writing effective tools for agents](https://www.anthropic.com/engineering/writing-tools-for-agents).

Anthropic's context guidance favors clear boundaries and retrieving relevant
information as needed, supporting discovery followed by targeted reading.
[Effective context engineering for AI agents](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents).

MCP recommends a serialized JSON text block alongside `structuredContent` for
compatibility. This implementation keeps both and reduces their underlying data.
Execution errors remain `isError` results with actionable feedback.
[MCP tools specification](https://modelcontextprotocol.io/specification/2025-11-25/server/tools).

## Findings in the previous implementation

- Listings defaulted to 200 complete Vela objects. Repeated IDs, hashes,
  timestamps, URLs, and descriptions consumed space when locating a path. A valid
  page could exceed the 128,000-byte default HTTP result budget and require a retry.
- `read_file` returned complete metadata alongside every content preview. Its
  automatic binary fallback returned base64 when the agent needed a PDF or image
  downloaded into its working environment.
- Revision descriptions suggested an extra metadata call even when an earlier
  listing, read, creation, or update had already returned the revision.
- HTTP upload schemas advertised `local_path`, although the client's attachment
  path is not accessible in the server container.
- Pagination guidance encouraged reading every page. Share-link expiry choices
  were mutually exclusive in business validation but not in the input schema.
- A response-size error after a successful mutation could look like a failed
  operation, inviting an unsafe retry.

## Accepted design

Only `search_files` is added: 31 tools total, 22 enabled by default in stdio and
24 in HTTP with the transfer broker. Individual configuration switches remain.

`list_directory` browses immediate children or recursive files;
`search_files` discovers paths using metadata; `read_file` reads known text;
`get_file_metadata` inspects full metadata; download tools transfer original
bytes. Ordinary operations do not require a `server_info` preflight.

Listings and reads default to `response_format="concise"`; `"detailed"` restores
complete metadata. Compact discovery records retain paths and revisions so an
agent can act without an automatic stat call. The default listing page is 50;
trash, policy, and share-link listings also use 50. Continue a cursor only when
the task needs more results, with unchanged filters. Stop when the task is done.

Reads return content and preview status without the full object by default.
Automatic encoding accepts text instead of silently returning binary base64.
Explicit `encoding="base64"` remains available for small byte inspection.
Preview fitting considers JSON escaping and both result representations in the
serialized envelope. Reported truncation preserves UTF-8 boundaries. Download
the original before editing a truncated file.

Source schemas expose only available capabilities. HTTP does not advertise
server-local upload paths. Share-link schemas reject simultaneous `ttl` and
`expires_at`. Previously returned revisions can be reused; concurrency guards
remain enforced and stale revisions require rereading and reconsidering the
change. An oversized successful mutation result becomes a small completed
receipt; uncertain network outcomes require inspection before another mutation.

## Search constraints

Vela currently has no search index endpoint. `search_files` performs a bounded
scan of recursive metadata listings: at most five REST pages and 500 objects per
invocation. It matches a literal, case-insensitive substring in path, title,
description, or tags. It downloads no content and provides neither full-text
nor semantic search. The default is 20 matches; the maximum is 100.

A response may be empty and still incomplete. `complete=false` and `next_cursor`
mean more candidates remain; only a complete scan can establish absence. Sparse
matches can require more backend calls than browsing. A narrow prefix reduces
that cost. Every fetched page is consumed before returning its upstream cursor,
so partial-page boundaries do not lose matches. Cursors
are bound to the query, filters, and caller, and do not provide a snapshot of a
concurrently changing store. Authorization is checked on every invocation.

## Alternatives deliberately excluded

Separate name, tag, title, grep, and content-only aliases would add overlapping
choices. Content search and bulk downloading would create unbounded upstream
work without an index. Fabricating pagination after discarding part of a page
would lose results. Universal plain-text output weakens machine-readable
receipts and compatibility.

## Evaluation and compatibility

Deterministic fixtures compare serialized MCP result bytes and tool/backend call
counts for browsing, short notes, Markdown, escaped HTML, and finding then
reading a file. Regression coverage includes binary rejection, explicit base64,
Unicode truncation, filters, scoped callers, cursor continuation, and schema
validation before mutations. These are byte and workflow measurements, not
model token counts or evidence of improved Claude accuracy. Model-driven
evaluation remains follow-up work.

| Fixture | Previous result bytes | New result bytes | Calls / outcome |
| --- | ---: | ---: | --- |
| Browse the same 50 files | 66,929 | 23,629 | One MCP call and one REST request in either case |
| Read a 14-byte note | 1,700 | 494 | One MCP call and two REST requests in either case |
| Read 3,840-byte Markdown | 9,741 | 8,535 | Same complete content; no metadata preflight |
| Preview 32,768-byte escaped HTML | 427,665 | 127,988 | New response fits the budget and reports a 9,809-byte prefix |
| Find then read one file | 135,176 | 1,599 | MCP calls 3 → 2; REST requests 4 → 7 |

The old 200-row default in the browsing fixture serialized to 266,709 bytes,
exceeding the default 128,000-byte HTTP budget. The HTML case measures a bounded
prefix rather than a smaller encoding of the complete source. In the discovery
fixture, search examined 99 candidates and returned a useful match while still
reporting an incomplete scan. This reduced client interaction and output but
increased backend requests, making the search tradeoff explicit. Tool discovery
definitions also occupy context; result-byte savings are not a measurement of
the entire conversation. An isolated comparison with the base commit `bfa4913`
measured default HTTP `tools[]` discovery at 22,457 bytes before and 24,876 bytes
after (23 to 24 tools, a 10.8% increase). The search definition contributes
1,334 bytes. Its cost is justified by a distinct discovery workflow; adding
several overlapping search/read aliases would increase this resident cost.

The tests reconstruct the legacy result contract and compare current SDK
results on the same deterministic data. Both sides include the SDK's current
server-info metadata and completion discriminator. Measurements include JSON
text and structured result representations, excluding HTTP headers. Reproduce
them with:

```bash
go test -v -run 'TestRetrievalPayloadEvaluation|TestFindThenReadWorkflowEvaluation' ./internal/mcpserver
```

Defaults change intentionally: compact results, smaller listing pages, and no
automatic binary base64. Consumers expecting the old object shape should request
`response_format="detailed"`; consumers needing encoded bytes must explicitly
request `encoding="base64"`. The Vela REST API, stored files, OAuth grants, and
SQLite schema are unchanged. No database migration is needed.
