BINARY   := forge
PKG      := ./cmd/forge
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
TARGETS  := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

.PHONY: build test test-web test-perf lint fmt check-schema release-snapshot clean demo docs-check quickstart-check

build:
	@mkdir -p dist
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY) $(PKG)

test: check-schema
	go test ./...

# Vercel dashboard (web/) tests: Node 22+ built-in runner, no npm install. Not part of `make test` (Go only).
test-web:
	cd web && node --test

# Strict time budgets (hook 2s, doctor/history limits) for an idle machine; the default
# run only has loose bounds so a busy host does not fail it. Serial packages (-p 1).
test-perf: check-schema
	FORGE_PERF_TESTS=1 FORGE_LARGE_TESTS=1 go test ./... -count=1 -p 1

# db/schema.sql (snapshot) must equal all migrations concatenated in order (ADR-F0001 D5; TASK-0013 changed the rule from "== 0001").
# Regenerate: cat db/migrations/*.sql > db/schema.sql
check-schema:
	@cat db/migrations/*.sql | cmp -s - db/schema.sql || (echo "db/schema.sql != concatenation of db/migrations/*.sql (cat db/migrations/*.sql > db/schema.sql)"; exit 1)

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

# Local demo in a fully isolated temp HOME/FORGE_HOME/project with fake claude/opencode
# binaries (scripts/demo/demo.sh). Never touches ~/.forge or real clients, no model calls.
demo: build
	FORGE_BIN="$(CURDIR)/dist/$(BINARY)" bash scripts/demo/demo.sh

# Static check of README.md and docs/: links, forge subcommands/flags that exist,
# invisible characters, no local-only paths. Does not execute the documented commands.
docs-check: build
	FORGE_BIN="$(CURDIR)/dist/$(BINARY)" bash scripts/docs-check.sh

# Runs the commands of docs/guide/quickstart.md (and the two client guides, including their
# `make build` step) for real, in an isolated temp HOME/FORGE_HOME/project with fake claude/opencode; prints timings.
quickstart-check:
	bash scripts/quickstart-check.sh
