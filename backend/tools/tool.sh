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
dir="${QUAI_TOOLS:-$HOME/.cache/quai-tools}/$(go env GOVERSION)/$name@$version"
if [ ! -x "$dir/$name" ]; then
	GOBIN="$dir" go install "$pkg"
fi
exec "$dir/$name" "$@"
