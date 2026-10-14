.PHONY: build test check docker
build:
	go build -buildvcs=false -trimpath -o bin/vela-mcp ./cmd/vela-mcp
test:
	go test -race ./...
check:
	go vet ./...
docker:
	docker build -t vela-mcp:dev .
