# syntax=docker/dockerfile:1
# Cross-compile on the builder's native platform; the final image is static and
# serves either stdio or HTTP. Keep Go aligned with go.mod and CI workflows.
FROM --platform=$BUILDPLATFORM golang:1.27.2-bookworm AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -buildvcs=false -trimpath \
      -ldflags "-s -w \
        -X go.xchunk.org/vela-mcp/internal/version.Version=${VERSION} \
        -X go.xchunk.org/vela-mcp/internal/version.Commit=${COMMIT} \
        -X go.xchunk.org/vela-mcp/internal/version.Date=${DATE}" \
      -o /out/vela-mcp ./cmd/vela-mcp
RUN mkdir -p /out/data && chmod 700 /out/data

FROM scratch
ARG VERSION=dev
ARG COMMIT=none
LABEL org.opencontainers.image.title="vela-mcp" \
      org.opencontainers.image.description="Vela MCP connector with stdio, HTTP and local OAuth" \
      org.opencontainers.image.source="https://github.com/x-chunk/vela-mcp" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/vela-mcp /vela-mcp
COPY LICENSE /usr/share/licenses/vela-mcp/LICENSE
COPY config.toml /app/config.toml
COPY --from=build --chown=65532:65532 /out/data /data
ENV VELA_MCP_OAUTH_DATABASE=/data/oauth.db \
    VELA_MCP_ENCRYPTION_KEY_FILE=/data/encryption.key \
    VELA_MCP_TRANSFER_DATABASE=/data/transfers.db
EXPOSE 8081
VOLUME ["/data"]
WORKDIR /app
USER 65532:65532
ENTRYPOINT ["/vela-mcp"]
