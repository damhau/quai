#!/bin/sh
# Tier 1 of docs/engineering/testing.md, the checks an iteration runs before committing (make
# quick). It builds and vets every package, then runs staticcheck and the tests (without -race:
# CI runs the full suite on every push) of the packages changed since the upstream branch,
# committed or not, and of those that import them; sqlc diff when the queries or migrations
# changed; quai lint on the example repository when it changed.
set -eu
cd "$(dirname "$0")/.."
base=$(git merge-base HEAD '@{upstream}' 2>/dev/null || git rev-parse HEAD)
changed=$({ git diff --name-only "$base"; git ls-files --others --exclude-standard --full-name; } | sort -u)
module=$(go list -m)

go build -o /dev/null ./...
go vet ./...

# The packages the changed files are in: a file's directory, or the package its testdata is for.
want=""
for f in $(printf '%s\n' "$changed" | sed -n 's|^backend/||p'); do
	dir=$(dirname "$f")
	dir=${dir%%/testdata*}
	if ls "$dir"/*.go >/dev/null 2>&1; then
		want="$want $module/$dir"
	fi
done
case "$changed" in
*backend/migrations/* | *backend/internal/store/queries/*)
	"${TOOL:-./tools/tool.sh}" "$SQLC" diff
	want="$want $module/internal/store"
	;;
esac
case "$changed" in
*examples/*)
	go run ./cmd/quai lint ../examples/gitops-repo
	want="$want $module/internal/api $module/internal/ingest $module/internal/engine/lint"
	;;
esac
if [ -z "$want" ]; then
	echo "quick: no Go package changed"
	exit 0
fi

# Those packages, and every package that imports them, or whose tests do.
pkgs=$(go list -f '{{.ImportPath}} {{join .Deps " "}} {{join .TestImports " "}} {{join .XTestImports " "}}' ./... |
	awk -v want="$want" 'BEGIN { n = split(want, w, " "); for (i = 1; i <= n; i++) W[w[i]] = 1 }
		{ for (i = 1; i <= NF; i++) if ($i in W) { print $1; next } }')
echo "quick: $(printf '%s\n' "$pkgs" | wc -l | tr -d ' ') packages"
"${TOOL:-./tools/tool.sh}" "$STATICCHECK" $pkgs
go test -count=1 $pkgs
