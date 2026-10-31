#!/bin/sh
# Runs a Go tool at a pinned version: tool.sh <module/cmd@version> [args...]. The tool is built
# once, with the toolchain this module selects, into $QUAI_TOOLS/<toolchain>/<name>@<version>
# (default ~/.cache/quai-tools), and run from there: CI keeps that directory between runs.
set -eu
pkg=$1
shift
name=${pkg##*/}
name=${name%@*}
version=${pkg##*@}
toolchain=$(go env GOVERSION)
dir="${QUAI_TOOLS:-$HOME/.cache/quai-tools}/$toolchain/$name@$version"
if [ ! -x "$dir/$name" ]; then
	# With that toolchain: go install pkg@version would otherwise pick the tool's own minimum
	# when the go on PATH is older, and the tool could not read this toolchain's packages.
	GOTOOLCHAIN=$toolchain GOBIN="$dir" go install "$pkg"
fi
exec "$dir/$name" "$@"
