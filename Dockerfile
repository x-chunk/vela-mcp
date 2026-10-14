# syntax=docker/dockerfile:1
# The build produces one runtime image, with one static stdio process.
FROM golang:1.27.2-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags='-s -w' -o /out/vela-mcp ./cmd/vela-mcp
RUN mkdir -p /out/data && chmod 700 /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/vela-mcp /vela-mcp
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
