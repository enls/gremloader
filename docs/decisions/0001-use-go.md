# 0001. Use Go

Status: accepted (2026-10-10)

## Context

The prototype (CraftWerk) is two code bases: PowerShell with Windows Forms, and bash. The app list, asset naming and Windows file-type registration are hard-coded in each. The replacement has to run on Windows and Linux, on amd64 and arm64, be easy to hand to another person, and be testable in CI.

## Decision

Write GremLoader in Go, as a single statically linked binary per OS and architecture.

## Reasons

- One file to distribute, no runtime to install on the target machine.
- Cross-compiles from one machine with `GOOS`/`GOARCH`, as long as nothing needs cgo.
- The standard library covers most of what the tool does: `net/http`, `archive/tar`, `archive/zip`, `crypto/sha256`, `html/template`, `embed`, `os/exec`.
- Good tooling for the quality gates: `go vet`, `staticcheck`, `govulncheck`, the race detector, built-in fuzzing.
- Sigstore's Go library (`sigstore-go`) is first party, which matters for ADR 0006.

## Consequences

- Dependencies stay few and are justified individually in an ADR line.
- cgo is off limits. Anything that needs a C compiler is rejected (see "Decided against" in the plan: the webui-dev library).
- Both licences, `go.sum` and the toolchain version are committed and pinned.

## Alternatives considered

- **Rust:** comparable result, steeper learning curve for this project's author, and the benefit (memory safety) matters less for a tool that mostly shells out and moves files.
- **Keep PowerShell + bash:** two code bases that drift, no unit-test story on Linux, and Windows PowerShell 5.1 quirks.
- **Python / Node:** needs a runtime on every target machine, which defeats "one file".
