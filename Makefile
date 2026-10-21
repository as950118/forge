BINARY   := forge
PKG      := ./cmd/forge
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
TARGETS  := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

.PHONY: build test lint fmt check-schema release-snapshot clean

build:
	@mkdir -p dist
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY) $(PKG)

test: check-schema
	go test ./...

# db/schema.sql (snapshot) must equal db/migrations/0001_init.sql while it is the only migration (ADR-F0001 D5).
check-schema:
	@cmp -s db/schema.sql db/migrations/0001_init.sql || (echo "db/schema.sql != db/migrations/0001_init.sql"; exit 1)

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; \
	else echo "golangci-lint not found; falling back to go vet + gofmt"; \
	go vet ./... && test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1); fi

fmt:
	gofmt -w .

release-snapshot:
	@mkdir -p dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-$$os-$$arch $(PKG) || exit 1; \
	done

clean:
	rm -rf dist
