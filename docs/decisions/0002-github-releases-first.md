# 0002. GitHub releases are the first source backend

Status: accepted (2026-10-10)

## Context

GremLoader will have several kinds of source: GitHub releases, OS package managers (apt, dnf,
winget) and Docker Compose. Each is close to a project of its own (privilege, service restarts,
rollback semantics).

## Decision

Define one backend interface (`Latest`, `Installed`, `Plan`, `Apply`, `Rollback`, `Remove`, `Health`,
`Capabilities`) and implement `github-release` first. The other backends are later milestones, not
features of the first one.

## Reasons

- It is the case the prototype already proved: the twelve Craft apps, installed from release assets.
  Phase 1 is accepted when GremLoader reproduces everything CraftWerk does from one package file.
- It exercises the hardest shared code first: download, verification, archive extraction,
  stage-then-place install, rollback, state and journal. The other backends reuse it.
- It is fully testable against a fake release server, with no real network.

## Consequences

- `Capabilities()` is part of the interface from the start. apt cannot reliably roll back and winget
  cannot always pin, so each backend states what it supports and the CLI and UI refuse the rest up
  front instead of failing mid-way.
- The interface is allowed to change while only one backend exists. It is frozen when the second
  one lands.
