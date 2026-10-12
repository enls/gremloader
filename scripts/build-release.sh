#!/usr/bin/env bash
# Build one release archive for one target.
#
#   scripts/build-release.sh <version> <goos> <goarch> <outdir>
#
# Writes <outdir>/gremloader-<version>-<goos>-<goarch>.tar.gz (zip for windows) holding the
# binary, both licenses and the README, and prints its path. Run from the repository root.
#
# The build is meant to be reproducible: no cgo, -trimpath, no VCS stamping by the toolchain, and
# every timestamp comes from the commit being built, so building the same commit twice gives the
# same archive bytes.
set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <version> <goos> <goarch> <outdir>" >&2
  exit 2
fi
version="${1#v}"
goos="$2"
goarch="$3"
outdir="$4"

pkg="github.com/enls/gremloader/internal/version"
commit="$(git rev-parse HEAD)"
date="$(git show -s --format=%cI HEAD)"
epoch="$(git show -s --format=%ct HEAD)"

name="gremloader-${version}-${goos}-${goarch}"
bin="gremloader"
[ "$goos" = "windows" ] && bin="gremloader.exe"

mkdir -p "$outdir"
outdir="$(cd "$outdir" && pwd)"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
root="$stage/$name"
mkdir -p "$root"

CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
  -trimpath -buildvcs=false \
  -ldflags "-s -w -X ${pkg}.Version=${version} -X ${pkg}.Commit=${commit} -X ${pkg}.Date=${date}" \
  -o "$root/$bin" ./cmd/gremloader

cp LICENSE-MIT LICENSE-APACHE README.md "$root/"
find "$root" -exec touch -d "@${epoch}" {} +

if [ "$goos" = "windows" ]; then
  out="$outdir/$name.zip"
  rm -f "$out"
  (cd "$stage" && zip -X -r -q "$out" "$name")
else
  out="$outdir/$name.tar.gz"
  # The binary's mode is set explicitly (0755), not read from the filesystem: a Windows host has
  # no exec bit, and the archive must come out the same wherever it is built. The binary sorts
  # last ("gremloader" after the upper-case names), so appending it keeps the sorted order.
  tarfile="$stage/$name.tar"
  common=(--owner=0 --group=0 --numeric-owner --mtime="@${epoch}")
  tar "${common[@]}" --sort=name -C "$stage" --exclude="$name/$bin" -cf "$tarfile" "$name"
  tar "${common[@]}" --mode=0755 -C "$stage" -rf "$tarfile" "$name/$bin"
  gzip -n -9 < "$tarfile" > "$out"
fi

echo "$out"
