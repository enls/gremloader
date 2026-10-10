# Security policy

GremLoader downloads software, unpacks archives, runs hooks and (in later phases) changes host
settings. Treat security reports about it as high priority.

## Reporting a vulnerability

Please **do not open a public issue** for a security problem. Use GitHub's private vulnerability
reporting: the repository's **Security** tab, then **Report a vulnerability**.

Include what you found, how to reproduce it, and the version (`gremloader version`). You can expect
an acknowledgement within a few days. This is a one-person project, so there is no formal SLA, but
reports are taken seriously and credited unless you ask otherwise.

## Supported versions

GremLoader is `0.x`. Only the latest release receives fixes.

## Areas of particular interest

- Archive extraction (path traversal, symlink and hardlink escapes, decompression bombs, reserved
  Windows names).
- Download and verification (checksum and attestation bypass, redirects to unexpected hosts, TLS).
- The local web UI (token and cookie handling, Host / Origin checks, CSRF, anything that lets a web
  page you visit trigger an install).
- Hooks and template rendering (anything that lets data from a release run as a command).
- Secrets handling (anything that writes a secret to the package file, lock file or log).
