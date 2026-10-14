# CI and releases

The workflows follow Vela's Docker Hub configuration and release policy. GoReleaser packages this project's single executable; Docker Buildx publishes the same Dockerfile used for local development. No auxiliary shell scripts or reverse proxies are required.

## Continuous integration

`.github/workflows/ci.yml` runs on every branch push and pull request. It calls `verify.yml`, which tagged releases also call before publishing. The verification jobs are independent and need only `contents: read`:

| Job | Checks |
| --- | --- |
| Test | Tracked Go source formatting, tidy modules, vet including integration tests, unit tests with the race detector, native build |
| Lint | Pinned golangci-lint, including the integration harness |
| Secrets | Redacted scans of all checked-out Git history and the clean source tree with pinned Gitleaks |
| Packages | GoReleaser configuration validation and a six-platform snapshot release without publication |
| Integration | Build the scratch image, provision a disposable Vela v0.2.0 container, run native workflows and Docker stdio/HTTP checks |

Integration creates fresh `admin`, `ro`, and `rw` keys through `velad key create`. Keys are masked before use and stored in a private file under the runner's temporary directory. The disposable backend, its volume, and the key file are removed in an `always()` cleanup step. No developer `.env`, API keys, persistent state, or registry credentials are needed. The tests exercise the actual browser consent/PKCE flow, per-user authority, tool operations, and verified binary transfers.

Pull requests build and test images but never log in to Docker Hub, publish packages, or access release credentials. Runtime execution is tested on Linux; macOS and Windows packages are cross-compiled, rather than claimed to have passed native runtime tests.

## Repository configuration

In GitHub repository **Settings → Secrets and variables → Actions**, configure the same names as Vela:

| Type | Name | Value |
| --- | --- | --- |
| Secret | `DOCKERHUB_USERNAME` | Docker Hub account used to publish the image |
| Secret | `DOCKERHUB_TOKEN` | Docker Hub access token with permission to push that repository |
| Variable | `DOCKERHUB_IMAGE` | Image repository, for example `xchunk/vela-mcp`, without a tag |

The binary-release job uses GitHub's automatic `GITHUB_TOKEN` with `contents: write` only in that job. No extra personal access token, GHCR credentials, or GoReleaser Pro license is required. Repository/organization policy must permit that job to create GitHub Releases. The verification workflow does not inherit secrets.

Tool versions are pinned. Keep the GoReleaser version aligned between `Makefile`, `verify.yml`, and `release.yml`; keep the golangci-lint version aligned between `Makefile` and `verify.yml`. Go setup reads `go.mod`; the Docker builder's Go version must remain compatible with it.

## Publishing a release

Push a SemVer tag such as `v0.3.0` or `v0.3.0-rc.1` on the reviewed commit. A branch push alone does not publish anything. For example, after the release changes have been reviewed and merged:

```bash
git tag -a v0.3.0 -m "Vela MCP v0.3.0"
git push origin v0.3.0
```

`release.yml` first runs the complete verification workflow. Only after it succeeds does the image job log in and publish a multi-platform manifest for `linux/amd64` and `linux/arm64`. The GitHub Release is published after that image job succeeds, with generated release notes and these artifacts:

- `vela-mcp_<version>_linux_{amd64,arm64}.tar.gz`
- `vela-mcp_<version>_darwin_{amd64,arm64}.tar.gz`
- `vela-mcp_<version>_windows_{amd64,arm64}.zip`
- `checksums.txt` containing SHA-256 checksums

Each archive has a containing directory and includes the executable, `LICENSE` (MIT), `config.toml`, `.env.example`, `README.md`, and technical documentation. The explicit archive allowlist excludes `.env`, real keys, SQLite databases, and development binaries. `dist/` is ignored by Git and excluded from the Docker context.

The Docker image includes the license at `/usr/share/licenses/vela-mcp/LICENSE` and advertises `MIT` through its OCI license label.

Stable `vX.Y.Z` image releases receive `X.Y.Z`, `X.Y`, and `latest` tags. Prereleases receive their full version tag, such as `0.3.0-rc.1`, and do not replace `latest` or the minor-version tag. GitHub prereleases are marked automatically. As in Vela, stable releases update floating image tags in publication order; pin a full version or digest for controlled deployments. Do not reuse or move published tags.

Version, full Git commit, and commit timestamp are embedded in release executables and Docker builds. `vela-mcp -version` displays the metadata; the MCP handshake advertises the same resolved version. Build dates come from the commit rather than the release runner's clock. OCI labels carry version/revision and the source repository.

## Local verification

```bash
make test check lint
make release-check
make release-snapshot
make docker
```

`release-check` and `release-snapshot` use the pinned open-source GoReleaser. They need a Git checkout with an `origin` URL so GoReleaser can identify the source repository; they do not publish. Snapshots work without a version tag and include `SNAPSHOT` in their versions. Generated archives and checksums are written to `dist/`.

You can use already installed tools instead of the pinned `go run` defaults:

```bash
make lint GOLANGCI_LINT=/absolute/path/golangci-lint
make release-check release-snapshot GORELEASER=/absolute/path/goreleaser
```

`make integration` needs `VELA_MCP_TEST_KEYS` pointing to a disposable Vela store's key file and optionally `VELA_MCP_TEST_URL`. It fails if the key-file variable is missing, instead of reporting success with skipped tests. See [deployment verification](deployment.md#verification) for the native/Docker test variables. Do not run the integration suite against production.

The two publication jobs are not one remote transaction. If a binary release fails after the image has been pushed, inspect the failed run and rerun the failed job; do not create a new tag merely to hide a partial release. Changes to workflows still require a real GitHub run to validate the hosted runner and repository permissions.
