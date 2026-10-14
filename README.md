# Vela MCP

A Go MCP server for [Vela API v1](https://docs.xchunk.org/vela/docs/api-reference.html), with **stdio** for local clients and **Streamable HTTP with local OAuth 2.1 authorization** for connectors. Authorization uses code flow with mandatory S256 PKCE and rotating refresh tokens. One Docker image contains the executable, configuration, and CA certificates. TLS termination belongs to your reverse proxy.

The 30 business tools cover files, metadata, visibility, moves, signed links, trash, directory policies, and binary HTTP transfers. The standard set exposes 21 tools over stdio and 23 over HTTP. Replacement, bulk deletion, root policy changes, and permanent deletion require explicit enablement.

## HTTP connector quick start

Requires Go 1.27.2+ and Vela v0.2.0 / API v1. Vela must be reachable from the server; users supply their own Vela API keys through the browser authorization page.

```bash
make build
cp .env.example .env
chmod 600 .env
# Set VELA_URL; HTTP mode does not use VELA_TOKEN.
./bin/vela-mcp -transport http \
  -listen 127.0.0.1:8081 -issuer http://127.0.0.1:8081
```

Use `http://127.0.0.1:8081/mcp` with a local development MCP client. A cloud connector needs a reachable HTTPS origin, for example `https://vela-mcp.example.com/mcp`; deploy the server behind your nginx using the [deployment guide](docs/deployment.md). Issuers must be root origins: deployment below a URL path prefix is unsupported.

The client discovers OAuth endpoints, registers a public client or presents an HTTPS client metadata document, and opens the authorization page. Enter a Vela API key and approve access. The server verifies the key with Vela and stores it encrypted in local SQLite. The connector receives short-lived OAuth tokens with that key's scope and path prefix; it never receives the Vela key. No local password account or shared administrator credential is required.

On first HTTP startup, the server creates `data/oauth.db`, `data/encryption.key`, and `data/transfers.db` by default. The encryption key is a private file containing 32 random bytes. Preserve it with the database; losing or replacing it makes stored Vela credentials unreadable. Existing storage with a missing or incorrect key fails startup.

## stdio quick start

```bash
# Set VELA_URL and VELA_TOKEN in the private .env file.
./bin/vela-mcp -transport stdio -check
./bin/vela-mcp -transport stdio -list-tools
./bin/vela-mcp -transport stdio
```

stdio verifies the Vela key at startup. stdout carries MCP messages; diagnostics go to stderr. Example MCP host configuration:

```json
{
  "mcpServers": {
    "vela": {
      "command": "/absolute/path/vela-mcp/bin/vela-mcp",
      "args": [
        "-transport", "stdio",
        "-config", "/absolute/path/vela-mcp/config.toml",
        "-env-file", "/absolute/path/vela-mcp/.env"
      ]
    }
  }
}
```

The same image supports stdio with `docker run --rm -i ... vela-mcp:dev -transport stdio`. Use `-i` without a TTY. See [native and Docker deployment](docs/deployment.md) for complete commands.

## Files and attachments

New files are create-only and explicitly default to **private**. Creation applies all writable metadata in the same upload: `visibility`, `title`, `description`, `tags`, `content_type`, `disposition`, and `expires_at`. Vela assigns IDs, hashes, sizes, revisions, and bookkeeping timestamps. Directories are implicit prefixes; no empty-directory creation is needed.

For HTTP connectors, use `prepare_file_upload` to bind a destination, metadata, size, and SHA-256. Send the original bytes from the client's computing environment to the returned PUT URL using the returned authorization header. `prepare_file_download` returns an expiring browser-usable URL for the original bytes of any file, including PDF, images, Office documents, archives, audio, and arbitrary binary formats. Read-only Vela keys can prepare downloads without changing private visibility. Check `get_file_transfer_status` after a lost response.

An HTTP connector alone does not transfer chat attachments. The client must perform the advertised HTTP request from its own computing environment. Its host must permit network access to your domain. Native attachments and file processing depend on the host's format and size support; an expiring link is the fallback. See the implemented [file-transfer contract](docs/http-file-transfer.md).

For stdio, `upload_file` accepts base64 or a process-visible absolute `local_path`; `download_file` writes to an allowed output directory. Configure `files.upload_roots` / `files.download_roots` and mount those directories into Docker when needed. An attached cloud file is not automatically present on the server's filesystem. HTTP deployments disable local filesystem access and omit `download_file`, even when stdio roots exist in the configuration.

Conversation previews default to 32 KiB. HTTP also caps each complete tool result at 128,000 serialized bytes, including text and structured representations, to keep connector responses bounded. Reduce `limit`/`max_bytes` when a result is too large, or download the original file through a transfer URL. Binary transfer limits default to 256 MiB independently of preview/result caps.

## Configuration

[config.toml](config.toml) contains typed, commented settings and independent tool switches. `.env` or process environment variables supply the upstream URL, stdio token, and deployment overrides. Process variables override `.env`, including explicitly empty values; environment settings override TOML; `-transport`, `-listen`, and `-issuer` override both.

```toml
[server]
transport = "http" # stdio remains the built-in default

[http]
listen = "127.0.0.1:8081"
issuer = "https://vela-mcp.example.com"

[tools]
read_file = true
prepare_file_upload = true
prepare_file_download = true
replace_file = false
purge_file = false
```

Omitted tools inherit defaults. `false` removes a tool from discovery and execution. Unknown names/fields and incorrect types fail startup. Restart to apply changes. Enabling a tool never grants additional Vela permissions. Setting `transfers.enabled=false` removes the broker routes and all three HTTP transfer tools.

Without flags, optional `./config.toml` and `./.env` are loaded. Use `-config` / `-env-file` or `VELA_MCP_CONFIG` / `VELA_MCP_ENV_FILE` for explicit files; those files must exist. Relative paths resolve against the working directory. `-list-tools` distinguishes configured enablement from transport availability. HTTP `-check` initializes and validates OAuth/transfer storage; it cannot verify a user's Vela key before browser authorization. `/healthz` checks the process, not Vela connectivity.

The chosen TOML plus environment configuration makes the tool inventory readable and keeps the stdio secret out of TOML. The tradeoff is two sources with documented precedence. Individual tool switches are intentionally not duplicated in environment variables.

## Development

```bash
make test
make check
make build
docker build -t vela-mcp:dev .
```

Tests cover authority isolation, schemas, switches, filesystem containment, OAuth consent/PKCE, token lifecycle/replay, encrypted persistence, Host/Origin policy, and verified binary transfers. Integration tests against a disposable Vela instance cover business workflows, real stdio and HTTP MCP clients, and file transfer. See [verification commands](docs/deployment.md#verification) for native/Docker options. Never point destructive integration scenarios at production.

See [tool contracts](docs/tools.md), [architecture](docs/architecture.md), [HTTP transfers](docs/http-file-transfer.md), and [deployment and operations](docs/deployment.md). HTTP deployment does not include nginx/Caddy, TLS certificates, password accounts, a grant-management dashboard, Vela key administration, or automatic delivery into a client's native attachment UI.
