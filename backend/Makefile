# Quality gates for the Go backend: docs/engineering/backend.md#quality-gates.
# Tools run at pinned versions through tools/tool.sh, which builds each once into a cache, so
# nothing needs installing.

STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOSEC       := github.com/securego/gosec/v2/cmd/gosec@v2.29.0
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.9.0
SQLC        := github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1

# `go install tool@version` picks a toolchain from the tool's go.mod, not ours. Build the tools
# with the toolchain this module selects, so they understand the standard library they analyse.
export GOTOOLCHAIN := $(shell go env GOVERSION)

TOOL := ./tools/tool.sh

.PHONY: check static build vet staticcheck gosec govulncheck generated test test-api test-others quick generate

check: static test

# The checks that do not run the tests: CI runs them in a job of their own.
static: build vet staticcheck gosec govulncheck generated

build:
	go build -o /dev/null ./...

vet:
	go vet ./...

staticcheck:
	$(TOOL) $(STATICCHECK) ./...

gosec:
	$(TOOL) $(GOSEC) -quiet -exclude-generated ./...

govulncheck:
	$(TOOL) $(GOVULNCHECK) ./...

# The code sqlc generates from internal/store/queries must be up to date.
generated:
	$(TOOL) $(SQLC) diff

test:
	go test -race -count=1 ./...

# The tests in two halves, CI's parallel jobs: internal/api, the slowest package, and the rest.
test-api:
	go test -race -count=1 ./internal/api

test-others:
	go test -race -count=1 $$(go list ./... | grep -v '/internal/api$$')

# Tier 1 of docs/engineering/testing.md: what an iteration runs before committing. Build and
# vet everything, then staticcheck and the tests of the packages changed since the upstream
# branch and of those that import them; sqlc diff when queries or migrations changed.
quick:
	STATICCHECK=$(STATICCHECK) SQLC=$(SQLC) ./tools/quick.sh

generate:
	$(TOOL) $(SQLC) generate
