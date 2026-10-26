# Quality gates for the Go backend: docs/engineering/backend.md#quality-gates.
# Tools run through `go run` at pinned versions, so nothing needs installing.

STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOSEC       := github.com/securego/gosec/v2/cmd/gosec@v2.29.0
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.9.0

# `go run tool@version` picks a toolchain from the tool's go.mod, not ours. Build the tools with
# the toolchain this module selects, so they understand the standard library they analyse.
export GOTOOLCHAIN := $(shell go env GOVERSION)

.PHONY: check build vet staticcheck gosec govulncheck test

check: build vet staticcheck gosec govulncheck test

build:
	go build -o /dev/null ./...

vet:
	go vet ./...

staticcheck:
	go run $(STATICCHECK) ./...

gosec:
	go run $(GOSEC) -quiet ./...

govulncheck:
	go run $(GOVULNCHECK) ./...

test:
	go test -race -count=1 ./...
