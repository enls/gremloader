#!/usr/bin/env bash
# Run the same gates CI runs, locally. CI is the real gate; this is the fast way to find out
# before pushing. Run from the repository root.
#
#   scripts/check.sh
#
# golangci-lint and govulncheck are skipped with a note if they are not installed. The race
# detector needs cgo and a C compiler, so it is only used when one is found.
set -uo pipefail

failed=0
step() { printf '\n== %s\n' "$1"; }
fail() { echo "FAILED: $1"; failed=1; }

step "gofmt"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "$unformatted"
  fail "gofmt"
fi

step "go vet"
go vet ./... || fail "go vet"

step "go mod tidy is a no-op"
snap="$(mktemp -d)"
cp go.mod "$snap/go.mod"
if [ -f go.sum ]; then cp go.sum "$snap/go.sum"; fi
go mod tidy
if ! cmp -s go.mod "$snap/go.mod" || { [ -f go.sum ] && ! cmp -s go.sum "$snap/go.sum" 2>/dev/null; }; then
  fail "go mod tidy changed go.mod or go.sum (the tidy result is now in the working tree)"
fi
rm -rf "$snap"

step "go test"
race=""
if [ "$(go env CGO_ENABLED)" = "1" ] && command -v gcc >/dev/null 2>&1; then race="-race"; else echo "(no C compiler: running without -race)"; fi
go test $race -count=1 ./... || fail "go test"

step "golangci-lint"
if command -v golangci-lint >/dev/null 2>&1; then
  golangci-lint run ./... || fail "golangci-lint"
else
  echo "skipped: golangci-lint not installed"
fi

step "govulncheck"
if command -v govulncheck >/dev/null 2>&1; then
  govulncheck ./... || fail "govulncheck"
else
  echo "skipped: govulncheck not installed (go install golang.org/x/vuln/cmd/govulncheck@latest)"
fi

echo
if [ "$failed" -ne 0 ]; then
  echo "CHECKS FAILED"
  exit 1
fi
echo "all checks passed"
