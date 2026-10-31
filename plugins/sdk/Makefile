GO ?= go

.PHONY: test vet check

test:
	$(GO) test ./...

vet:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt: the files above are not formatted"; exit 1; }
	$(GO) vet ./...
	GOOS=wasip1 GOARCH=wasm $(GO) vet ./...

check: vet test
