# The plugin SDK, the template and the example INI plugin: three modules, each checked on its own
# (gofmt, go vet natively and for wasip1, the tests, and the plugins' builds), and staticcheck at
# the backend's pinned version, native and wasip1. CI runs `make -C plugins check`
# (.github/workflows/plugins.yml); docs/engineering/plugins.md explains the modules.

MODULES := sdk template ini
STATICCHECK := $(shell sed -n 's/^STATICCHECK *:= *//p' ../backend/Makefile)
TOOL := $(CURDIR)/../backend/tools/tool.sh

.PHONY: check clean

check:
	@set -e; for m in $(MODULES); do \
		$(MAKE) -C $$m check; \
		(cd $$m && $(TOOL) $(STATICCHECK) ./... && GOOS=wasip1 GOARCH=wasm $(TOOL) $(STATICCHECK) ./...); \
	done

clean:
	@for m in template ini; do $(MAKE) -C $$m clean; done
