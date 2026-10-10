# 0003. The web UI is plain net/http, server-rendered, embedded in the binary

Status: accepted (2026-10-10)

## Context

The tool needs a graphical interface that works on a Windows PC and, through an SSH tunnel, on a
headless Linux server. A terminal menu was chosen first and then replaced.

## Decision

`gremloader ui` starts an HTTP server on `127.0.0.1` (random port) and opens the browser. Pages are
server-rendered with `html/template`, enhanced with htmx and server-sent events for live logs, and
embedded with `go:embed`. htmx is vendored into the binary.

## Reasons

- Pure Go, so one-command cross-compiling still works.
- One file to hand someone, no frontend build step, almost no hand-written JavaScript.
- Works on a machine with poor or filtered internet: no CDN, and a strict Content-Security-Policy
  with no external hosts.
- The UI is only a client of the same core the CLI and scheduled runs use. Closing it never affects
  scheduled updates.

## Rejected

- **webui-dev/webui:** its Go wrapper uses cgo, needs an installed browser at runtime (servers have
  none), and is beta with an incomplete Go API.
- **A CDN for htmx or fonts:** breaks offline use and the CSP.
- **A single-page app with a JS build:** adds a toolchain and a supply chain for a tool whose job is
  to be trusted.

## Consequences

- Security is part of the design, not an afterthought: loopback only, launch token exchanged for an
  HttpOnly SameSite=Strict cookie, Host and Origin checks, CSRF tokens, strict CSP, no CORS. Each is
  covered by handler tests that must refuse the bad request.
- The browser never sends a path or a command; actions are validated server-side against the
  package file.
