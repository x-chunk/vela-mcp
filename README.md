# Vela MCP

A Go MCP server for [Vela API v1](https://docs.xchunk.org/vela/docs/api-reference.html). The prototype serves **stdio**, ships as one Docker image, and authenticates with a Vela Bearer key. Its 27 business tools cover files, metadata, visibility, moves, signed links, trash, and directory policies. 21 tools are enabled by default; content replacement, bulk deletion, root policy changes, and permanent deletion are disabled.

## Quick start

Requires Go 1.27.2+ and Vela v0.2.0 / API v1.

```bash
make build
cp .env.example .env
chmod 600 .env
# Set VELA_TOKEN and VELA_URL in .env.
./bin/vela-mcp -check
./bin/vela-mcp -list-tools
./bin/vela-mcp
```

The last command waits for MCP messages on stdin. stdout is reserved for the protocol; diagnostics go to stderr. `-check`, `-list-tools`, and `-version` are separate commands that print a result and exit. Normal startup verifies the key and API compatibility before serving MCP. Use an `rw` key for everyday work, preferably restricted to a path prefix. An `ro` key supports file reading; trash and signed links require `rw`; permanent deletion requires `admin`.

Settings live in [config.toml](config.toml). Supply the token only through `.env` or the process environment. URL precedence is: process `VELA_URL` → `.env` → TOML → built-in default. Process variables also override the token from `.env`, including an explicitly empty value.

```bash
./bin/vela-mcp -config /absolute/path/config.toml -env-file /absolute/path/.env
```

Without flags, optional `./config.toml` and `./.env` are loaded. You can select paths with `VELA_MCP_CONFIG` and `VELA_MCP_ENV_FILE`; explicitly selected files must exist. Relative paths resolve against the process working directory.

## Connect a stdio client

Example configuration for an MCP host using `mcpServers`:

```json
{
  "mcpServers": {
    "vela": {
      "command": "/absolute/path/vela-mcp/bin/vela-mcp",
      "args": [
        "-config", "/absolute/path/vela-mcp/config.toml",
        "-env-file", "/absolute/path/vela-mcp/.env"
      ]
    }
  }
}
```

You can also supply the token through the host's `env` field instead of `.env`. The MCP resource `vela://guide` provides a short agent-facing usage guide. Tool names, descriptions, and schemas are in English; paths and metadata support Unicode.

## Docker

```bash
docker build -t vela-mcp:dev .
# Linux: Vela is reachable on the host's 127.0.0.1:8080.
docker run --rm -i --network host \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  --env-file "$PWD/.env" \
  -v "$PWD/config.toml:/app/config.toml:ro" \
  vela-mcp:dev
```

Use `-i` for stdio. A TTY (`-t`) is unnecessary. The runtime image contains only a static executable, CA certificates, and default configuration, running as UID/GID `65532`. Tokens and `.env` are excluded from the Docker build context. The image opens no listening port and contains no MCP database or additional service.

For Vela on a shared Docker network, use `VELA_URL=http://vela-dev:8080` and `--network <network>`. Docker Desktop typically uses `http://host.docker.internal:8080`; reachability depends on Vela's host bind address. The example above is tested on Linux with host networking.

Example Docker configuration for an MCP client:

```json
{
  "mcpServers": {
    "vela": {
      "command": "docker",
      "args": [
        "run", "--rm", "-i", "--network", "host",
        "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
        "--env-file", "/absolute/path/vela-mcp/.env",
        "-v", "/absolute/path/vela-mcp/config.toml:/app/config.toml:ro",
        "vela-mcp:dev"
      ]
    }
  }
}
```

## Files, attachments, and metadata

`create_text_file` creates a new UTF-8 file. `upload_file` accepts exactly one source: `content_base64` or an absolute `local_path` visible to the MCP process. Both apply **every writable metadata field during creation**: `visibility`, `title`, `description`, `tags`, `content_type`, `disposition`, and `expires_at`. Vela assigns the ID, SHA-256, size, revision, and bookkeeping timestamps.

```json
{
  "path": "notes/report.md",
  "text": "# Report\nReady for review.\n",
  "metadata": {
    "visibility": "private",
    "title": "Report",
    "description": "Review copy",
    "tags": ["report", "review"],
    "content_type": "text/markdown; charset=utf-8",
    "disposition": "inline",
    "expires_at": "7d"
  }
}
```

New files default to **private** and never overwrite an existing path. `replace_file` must be enabled separately and requires `expected_revision`. Directories are implicit file path prefixes; Vela has no separate `mkdir` operation or empty directories.

An attachment in a Claude conversation is not automatically accessible to a stdio server. The host must supply its bytes as base64 or place the file in a directory accessible to the MCP process. Prefer local paths for large files: base64 consumes more space and tokens. Arbitrary HTTP URLs and `file://` URIs are not accepted.

Local access is unavailable unless roots are configured. To enable local upload and download:

```toml
[files]
upload_roots = ["/files/in"]
download_roots = ["/files/out"]
```

Directories must already exist. In Docker, mount the input directory read-only and the output directory writable, then supply paths **inside the container**:

```bash
docker run --rm -i --network host \
  --user "$(id -u):$(id -g)" \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  --env-file "$PWD/.env" \
  -v "$PWD/config.toml:/app/config.toml:ro" \
  -v "$PWD/inbox:/files/in:ro" \
  -v "$PWD/outbox:/files/out" \
  vela-mcp:dev
```

`os.Root` confines access, including symlink escapes. Only regular files can be uploaded. `download_file` creates a new file with mode `0600`, verifies SHA-256, and removes an incomplete result on error. Its parent directory must exist. Default limits are 256 MiB for streaming uploads/downloads, 8 MiB for decoded inline input, and 1 MiB for conversation reads. Adjust these in TOML; Vela's limits also apply. A bounded `read_file` can return `truncated=true`; use `download_file` for a complete file.

## Enable or disable tools

```toml
[tools]
read_file = true
create_text_file = false
replace_file = false
purge_file = false
```

Every tool has an independent boolean. Omitted entries inherit the standard defaults; unknown names/fields and incorrect types fail startup. `false` removes the tool from `tools/list` and makes it unavailable even by a known name. Restart to apply changes; hot reload is not implemented. Enabling a tool never grants additional Vela key permissions.

The chosen configuration is **TOML plus environment secrets**: TOML provides comments, explicit types, and a readable tool inventory; environment variables are convenient for Docker and keep the token out of TOML. The tradeoff is two sources with explicit precedence. Using only `.env` is simpler to pass into a container, but dozens of switches are harder to read and easier to misspell. Individual tool switches are not duplicated in environment variables.

See the complete [tool catalog](docs/tools.md) for permissions, arguments, and semantics, and [architecture notes](docs/architecture.md) for boundaries and the future OAuth 2.1/HTTP integration.

## Development and verification

```bash
make test
make check
```

Unit/protocol tests cover configuration precedence, unknown switches, scopes and prefixes, identity changes between requests, safe creation/replacement, UTF-8/binary reads, filesystem containment, download cleanup, and disabled tools in the protocol.

Integration tests run separately against a **disposable** Vela store. The key file uses lines `admin: <key>`, `ro: <key>`, and `rw: <key>`. Keys are not saved in the repository or printed by tests.

```bash
make build
VELA_MCP_TEST_URL=http://127.0.0.1:8080 \
VELA_MCP_TEST_KEYS=/absolute/path/to/test/.keys \
VELA_MCP_TEST_BINARY="$PWD/bin/vela-mcp" \
go test -tags=integration -v ./test/integration

# Only the stdio smoke test, through Docker:
VELA_MCP_TEST_KEYS=/absolute/path/to/test/.keys \
VELA_MCP_TEST_IMAGE=vela-mcp:dev \
go test -tags=integration -run TestStdio -v ./test/integration
```

The integration scenario invokes all 27 tools with `admin`, checks `ro`/`rw` rejections, metadata on creation, stale revisions, Unicode paths, binary/local transfers, pagination, links after moves, and restore. It uses unique prefixes and removes its data. A temporary root policy title change is restored; bulk purge is tested with a 520-week cutoff. Do not run this scenario against production. The real subprocess verifies stdio initialization, discovery, creation, reading, and deletion; accidental stdout diagnostics would break this test.

HTTP, OAuth endpoints, MCP accounts, MCP SQLite persistence, Vela API/signing key administration, backups, storage migration, and recursive local-directory upload are not implemented. Local `velad` administration commands are outside the public REST API.
