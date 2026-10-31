# Builds the plugin as Quai runs it, a WebAssembly reactor for WASI preview 1, and prints the
# digest to pin in the type's spec.generate.digest. The build is reproducible: the same sources
# and Go version give the same digest (-trimpath and -buildvcs=false leave out the paths and the
# commit), so a reviewer can rebuild the module and compare.

PLUGIN ?= plugin.wasm
GO ?= go
SHA256 := $(shell command -v sha256sum 2>/dev/null || echo shasum -a 256)

.PHONY: build digest test vet check clean

build: $(PLUGIN) digest

$(PLUGIN): go.mod $(wildcard *.go) $(wildcard ../sdk/*.go)
	GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -buildmode=c-shared -o $@ .

digest: $(PLUGIN)
	@echo "sha256:$$($(SHA256) $(PLUGIN) | cut -d' ' -f1)"

# The golden cases in testdata, run natively: no WebAssembly needed.
test:
	$(GO) test ./...

vet:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt: the files above are not formatted"; exit 1; }
	$(GO) vet ./...
	GOOS=wasip1 GOARCH=wasm $(GO) vet ./...

check: vet test build

clean:
	rm -f $(PLUGIN)
