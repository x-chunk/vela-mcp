# syntax=docker/dockerfile:1
# The build produces one runtime image, with one static stdio process.
FROM golang:1.27.2-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags='-s -w' -o /out/vela-mcp ./cmd/vela-mcp

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/vela-mcp /vela-mcp
COPY config.toml /app/config.toml
WORKDIR /app
USER 65532:65532
ENTRYPOINT ["/vela-mcp"]
