# Deployment and operations

## Public origin and connectivity

Choose a dedicated origin such as `https://vela-mcp.example.com`. The connector URL is **`https://vela-mcp.example.com/mcp`**. Configure that origin as `http.issuer`; do not include `/mcp`, a deployment prefix, query or fragment. The server uses it for OAuth discovery, redirects and transfer links, regardless of `Forwarded` / `X-Forwarded-*` headers.

The executable serves plain HTTP locally. Your nginx terminates TLS and proxies the entire origin, including discovery, OAuth and binary routes. Keep the listener private or published on loopback. HTTPS is mandatory for a public issuer; loopback HTTP is supported for local development only. A cloud client's `localhost` is not your machine.

The server needs access to Vela's configured `VELA_URL`. HTTPS client metadata documents also require outbound DNS and HTTPS to public hosts; private/reserved targets and redirects are rejected. A client's computing environment must be able to reach this origin to perform file transfers, separately from its MCP connector connection.

## Native process

```bash
make build
cp .env.example .env
chmod 600 .env
mkdir -p data
chmod 700 data
# Set VELA_URL in .env. HTTP mode ignores VELA_TOKEN.
./bin/vela-mcp -config config.toml -env-file .env \
  -transport http -listen 127.0.0.1:8081 \
  -issuer https://vela-mcp.example.com
```

Run as a dedicated unprivileged account. Relative storage paths use the working directory, so use absolute paths in a managed service. Example production configuration additions:

```toml
[server]
transport = "http"

[http]
listen = "127.0.0.1:8081"
issuer = "https://vela-mcp.example.com"
allowed_origins = []
max_request_bytes = 52428800 # 50 MiB, MCP JSON; not binary transfer bodies
max_tool_result_bytes = 128000 # complete serialized tool result budget

[oauth]
database_path = "/var/lib/vela-mcp/oauth.db"
encryption_key_file = "/var/lib/vela-mcp/encryption.key"
access_ttl = "15m"
refresh_ttl = "720h" # absolute grant lifetime, not extended by refresh
code_ttl = "5m"
allowed_redirect_uris = []

[transfers]
enabled = true
database_path = "/var/lib/vela-mcp/transfers.db"
ttl = "15m"
```

Keep the existing `[vela]`, `[files]` and `[tools]` sections when editing `config.toml`. The state directory must be writable by the service account with mode `0700`. First startup creates a raw 32-byte encryption key with mode `0600`; do not write a text/password/base64 string into that file.

Example systemd unit, assuming the `vela-mcp` account, binary, config and state directory have already been provisioned:

```ini
[Unit]
Description=Vela MCP HTTP connector
After=network-online.target
Wants=network-online.target

[Service]
User=vela-mcp
Group=vela-mcp
WorkingDirectory=/opt/vela-mcp
ExecStart=/usr/local/bin/vela-mcp -config /etc/vela-mcp/config.toml -env-file /etc/vela-mcp/vela.env
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/vela-mcp
UMask=0077
TimeoutStopSec=20

[Install]
WantedBy=multi-user.target
```

The private environment file supplies `VELA_URL` and any overrides; it need not contain a global Vela key for HTTP. stdout is not a protocol channel in HTTP mode; diagnostics remain on stderr. SIGTERM/SIGINT drains requests for up to 15 seconds.

## Docker HTTP

The image runs as UID/GID `65532`, contains no shell or reverse proxy, and uses the same executable for both transports. Its SQLite/key defaults point into `/data`. Only that directory needs to be writable in HTTP mode.

```bash
docker build -t vela-mcp:dev .
# Initialize a persistent bind-mounted state directory for the image user.
sudo install -d -m 0700 -o 65532 -g 65532 "$PWD/data-http"

docker run -d --name vela-mcp --restart unless-stopped \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  -p 127.0.0.1:8081:8081 \
  -e VELA_MCP_TRANSPORT=http \
  -e VELA_MCP_LISTEN=0.0.0.0:8081 \
  -e VELA_MCP_ISSUER=https://vela-mcp.example.com \
  -e VELA_URL=http://host.docker.internal:8080 \
  --add-host host.docker.internal:host-gateway \
  -v "$PWD/config.toml:/app/config.toml:ro" \
  -v "$PWD/data-http:/data" \
  vela-mcp:dev
```

This example uses a host Vela service reachable from Docker's host-gateway address. A Vela process bound only to host loopback is not reachable through that address. On Linux, an alternative is `--network host` with `VELA_URL=http://127.0.0.1:8080`, `VELA_MCP_LISTEN=127.0.0.1:8081`, and no `-p` or host-gateway flag. On a shared Docker network, use `--network <network>` and `VELA_URL=http://vela-dev:8080`. Select the URL/network matching your actual Vela deployment.

A named volume also works, provided its ownership is initialized to `65532:65532` before startup. Do not keep databases or the encryption key only in a container's disposable writable layer. No tmpfs or `/tmp` mount is required: binary spools live in `/data/transfer-spool` by default.

For a local-only HTTP test, set issuer to `http://127.0.0.1:8081`. With a public issuer and a wildcard container bind, an internal health probe must preserve the public Host:

```bash
curl -fsS -H 'Host: vela-mcp.example.com' http://127.0.0.1:8081/healthz
docker logs vela-mcp
```

## nginx

The following is an operator-managed example. Install/configure nginx and certificates outside the image. Place `log_format` in the nginx `http` context; use your actual hostname and certificate paths.

```nginx
# Deliberately excludes query strings and Authorization headers.
log_format vela_mcp '$remote_addr [$time_local] '
                    '"$request_method $uri $server_protocol" '
                    '$status $body_bytes_sent';

server {
    listen 443 ssl;
    server_name vela-mcp.example.com;
    ssl_certificate /etc/letsencrypt/live/vela-mcp.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/vela-mcp.example.com/privkey.pem;

    access_log /var/log/nginx/vela-mcp.access.log vela_mcp;
    # nginx error logs can include request URLs. Disable request-level error
    # logging for this capability origin unless your logging layer redacts it.
    error_log /dev/null;

    client_max_body_size 256m;
    client_body_timeout 120s;

    location / {
        proxy_pass http://127.0.0.1:8081;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
        proxy_set_header Connection "";
        proxy_buffering off;
        proxy_request_buffering off;
        proxy_read_timeout 120s;
        proxy_send_timeout 120s;
        add_header Strict-Transport-Security "max-age=31536000" always;
    }
}

server {
    listen 80;
    server_name vela-mcp.example.com;
    access_log off;
    error_log /dev/null;
    return 301 https://vela-mcp.example.com$request_uri;
}
```

Keep all route paths unchanged and preserve Host. Avoid an external authentication redirect in front of discovery/token/MCP endpoints: the connector authenticates with OAuth at this server. Do not log `$args`, `$request_uri`, raw `$request`, bearer headers, request bodies, or transfer URLs in observability/APM middleware. Download URLs intentionally contain an expiring bearer capability in `token`; OAuth codes and client state also appear in redirects. If error logging is required, use a pipeline that strips queries before storage.

The server's own OAuth limiter uses the immediate peer address and ignores forwarded IP headers. Behind one nginx, its limits apply to the combined proxy traffic (registration: 10/hour; authorization: 30/minute; consent: 10/minute; token/revocation: 120/minute per peer). Add nginx limits by actual client IP appropriate to your load; they do not remove this aggregate application limit. Large multi-user registration bursts may require a future trusted-proxy rate-limit adapter.

## Connector authorization

Add `https://vela-mcp.example.com/mcp` as a Streamable HTTP MCP connector in your host. The client should:

1. Discover `/.well-known/oauth-protected-resource/mcp` and `/.well-known/oauth-authorization-server`.
2. Use public dynamic registration at `/oauth/register` or a validated HTTPS client ID metadata document. No client secret is supported.
3. Start `/oauth/authorize` with `response_type=code`, a registered redirect, `resource=https://vela-mcp.example.com/mcp`, `scope=vela`, and S256 PKCE. HTTPS redirects match exactly. Native loopback HTTP callbacks may vary only their port, following RFC 8252; hostname, path and query stay bound to registration. The accepted concrete callback must match exactly during token exchange.
4. Open the consent page, where the user enters and approves a Vela API key.
5. Exchange the code at `/oauth/token` with its PKCE verifier, exact client/redirect/resource binding, and use `Authorization: Bearer <access_token>` on `/mcp`.
6. Rotate refresh tokens as they are used. The absolute grant lifetime defaults to 30 days; then browser authorization is required again.

Machine clients generally omit Origin. Protected browser requests default to the issuer's Origin only; add a specific trusted HTTPS Origin to `http.allowed_origins` if a browser-based host sends a different one. Do not add wildcards. Discovery, registration, token and revocation endpoints allow public CORS without credential cookies. `oauth.allowed_redirect_uris`, when nonempty, is an exact optional redirect allowlist for registered/metadata clients; an empty list still enforces the server's URI validation.

HTTP does not use `VELA_TOKEN`, even if the environment contains one. Use `ro` keys for read-only workflows and prefix-restricted `rw` keys for ordinary upload/edit work. Configured switches limit the exposed tools but do not expand a user's key rights. Each consent has its own grant; authorizing the same key twice does not merge token identities.

Actual cloud-host compatibility must be checked in your account after exposing HTTPS. Automated tests validate the local OAuth/MCP/byte flows; they do not prove a particular Claude account, connector UI, network allowlist, or native attachment format can complete every workflow.

## Configuration reference

| Environment variable | TOML setting / purpose |
| --- | --- |
| `VELA_URL` | `vela.url`, upstream address |
| `VELA_TOKEN` | stdio credential only; absent from TOML |
| `VELA_MCP_TRANSPORT` | `server.transport`: `stdio` or `http` |
| `VELA_MCP_LISTEN` | `http.listen` |
| `VELA_MCP_ISSUER` | `http.issuer`, public root origin |
| `VELA_MCP_OAUTH_DATABASE` | `oauth.database_path` |
| `VELA_MCP_ENCRYPTION_KEY_FILE` | `oauth.encryption_key_file` |
| `VELA_MCP_TRANSFER_DATABASE` | `transfers.database_path` |
| `VELA_MCP_CONFIG` | Explicit TOML file path |
| `VELA_MCP_ENV_FILE` | Explicit dotenv file path |

Process environment overrides dotenv, which overrides TOML. The three transport/listen/issuer CLI flags override configuration. Unknown TOML fields/tools are rejected. Relative paths follow the working directory. Restart after changes; no hot reload exists.

The MCP JSON request limit defaults to 50 MiB. HTTP tool results have a separate 128,000-byte budget (`http.max_tool_result_bytes`) including text and structured representations. Oversized results return an actionable tool error: reduce page `limit` / read `max_bytes`, or use a binary download transfer. Inline input defaults to 8 MiB decoded, conversation reads to 32 KiB, and binary upload/download limits to 256 MiB each. Raising `files.max_read_bytes` does not bypass HTTP result limits. Change binary `[files]` limits and nginx body limits together; the upstream Vela limit also applies. `vela.timeout` defaults to 120 seconds and bounds each tool/request, including spooling. Increase it and nginx timeouts together for slower transfers. Transfer TTL defaults to 15 minutes, configurable from one minute to one hour.

## State, backup and revocation

Run **one instance per state directory**. Do not share the SQLite files between active replicas. OAuth uses a private key file and database; transfers use a separate database referring only to OAuth grant subjects. Preserve all three files together, including SQLite WAL/SHM files if using a live SQLite-aware backup.

For a simple consistent backup, stop the process, copy the state directory and private configuration to protected storage, then restart. The private `transfer-spool` directory is temporary and can be omitted from an offline backup. Restore the original encryption key with the corresponding database before startup, keep ownership/modes intact, and preserve the public issuer. A missing/wrong key fails startup. Do not generate a new key for an existing authorization database; key rotation requires a dedicated migration which is not implemented.

`POST /oauth/revoke` accepts a public `client_id`, a current access or refresh `token`, and optional `token_type_hint`. Revocation invalidates the entire grant and its prepared transfers. Correctly bound authorization-code replay and refresh-token replay also revoke the grant. Clients must save the new refresh token and never reuse an older one. Revoking the Vela key prevents further upstream operations as well. Removing a connector from a host is not necessarily an OAuth revocation request; verify the host's behavior.

Binary spooling allows four concurrent transfers. Budget at least four times the larger configured file limit plus SQLite/log space. There are at most 64 pending capabilities per grant; downloads allow 32 GET/HEAD uses, including retries/ranges. Transfer records remain available for seven days and are cleaned during later preparation. Capabilities expire sooner and become invalid immediately when their OAuth grant is revoked/expired. Restart cleans stale spools and marks interrupted upload claims uncertain. Inspect status/destination instead of assuming an interrupted upload failed.

## stdio Docker and local roots

```bash
docker run --rm -i --network host \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  --env-file "$PWD/.env" \
  -v "$PWD/config.toml:/app/config.toml:ro" \
  vela-mcp:dev -transport stdio
```

This Linux example expects Vela on host loopback. stdio needs `VELA_TOKEN`, creates no OAuth/transfer database, and opens no listener. Configure existing absolute upload/download roots and mount them separately when using local files. Paths passed in tool calls are container paths. Use a matching `--user` or directory ownership for writable output; files are created privately without overwrite. HTTP mode leaves these roots unavailable regardless of configuration.

## Verification

```bash
make test
make check
make build
VELA_MCP_TEST_URL=http://127.0.0.1:8080 \
VELA_MCP_TEST_KEYS=/absolute/path/to/disposable-vela/.keys \
VELA_MCP_TEST_BINARY="$PWD/bin/vela-mcp" \
go test -tags=integration -v ./test/integration

docker build -t vela-mcp:dev .
VELA_MCP_TEST_URL=http://127.0.0.1:8080 \
VELA_MCP_TEST_KEYS=/absolute/path/to/disposable-vela/.keys \
VELA_MCP_TEST_IMAGE=vela-mcp:dev \
go test -tags=integration -v ./test/integration
```

The key file contains `admin: <key>`, `ro: <key>`, and `rw: <key>` lines. Tests skip external scenarios when their required environment is missing. They never print the keys. Integration workflows create unique prefixes, exercise mutations and cleanup, and may temporarily change/restore root policy; use a disposable store. Local HTTP tests drive real consent/PKCE, SDK MCP calls and original binary bytes. Docker smoke tests verify the packaged process, writable state and both transports.

For deployed readiness, request `/healthz`, discovery, and an unauthenticated `/mcp` call: the latter should return 401 with the resource metadata challenge. Then perform a connector authorization and a small upload/download with that user's key. Health alone does not validate Vela connectivity, TLS trust, client callbacks or outbound file access.
