# webpty product and repository specification

## Purpose

webpty is a self-hosted terminal collaboration service for macOS and Linux.
One Go process serves the web application and manages local PTY child
processes. Remote execution, native Windows support, and a multi-node control
plane are outside the current product boundary.

This document is the canonical product and repository specification. Practical
installation and operations guidance belongs in the task-focused documents
under `docs/`.

## Product model

A deployment has one administrator. A new installation starts with the
password `CHANGEME` and blocks all other authenticated operations until the
administrator replaces it.

Each terminal workspace has one owner, no more than one invited editor, and any
number of read-only viewers. Sharing uses expiring, revocable capability links.
The plaintext capability is displayed only at creation; persistence stores
only its cryptographic hash, scope, expiry, revocation state, and audit
metadata.

The administrator uses `/admin` to control sessions, recordings, access
grants, effective settings, and audit history.

## Runtime architecture

The application is distributed as one Go binary. The supported toolchain is
Go 1.26.8, pinned in `go.mod`; release builders and validation must use the
same patch release. Standard-library `net/http` serves the versioned `/api/v1`
API, operational endpoints such as `GET /healthz`, WebSockets, and embedded
frontend assets.

SQLite is the system of record for administrator state, sessions, workspace
authorization, capabilities, terminal metadata, recordings, and audit events.
Schema changes use ordered migrations and transactions. Backups must include
the database and any external recording data introduced in the future.

The React and TypeScript client uses Vite and xterm.js. Production assets are
embedded into the Go binary and require no CDN. Development may run Vite
separately.

## Terminal, collaboration, and recording

The server owns PTY creation, process lifecycle, dimensions, authorization, and
output fan-out. Browser disconnects update presence without implicitly ending
the terminal.

The owner and current editor may send input. Viewers are read-only. Every API
request and WebSocket connection is authorized on the server, and revocation
takes effect for active guests.

Recordings contain ordered terminal output, resize, lifecycle, and participant
presence events sufficient for deterministic playback and asciicast export.
Exact keyboard input is never recorded. Keystrokes and input payloads must not
enter recordings, logs, analytics, audit details, or error messages.

## Security requirements

- Passwords use a memory-hard password hash.
- `CHANGEME` is rejected after the mandatory first-run rotation.
- Capability validation checks a hash of the presented token, scope, expiry,
  usage restrictions, and revocation.
- Authentication cookies, origin checks, transport behavior, and proxy
  handling use secure self-hosting defaults.
- Commands are executed directly rather than through a shell.
- PTY processes receive an allowlisted environment.
- SQLite constraints and transactions preserve authorization invariants.
- Sensitive values are excluded from logs, errors, recordings, and audit
  metadata.
- Network exposure requires TLS termination and an explicitly configured
  public origin.

## Supported distribution

Release artifacts target macOS and Linux on amd64 and arm64. Supported
installation paths are signed release archives, the checksum-verifying install
script, Homebrew, Docker, and source builds. Windows users may use WSL or
Docker.

Releases include checksums, SBOMs, and Sigstore signatures. CI verifies Go
tests with the race detector, static checks, frontend linting and type safety,
browser tests, cross-platform builds, vulnerability checks, secret scans, and
release archive installation.

## Open-source repository model

The project is maintainer-led with a lightweight community contribution
process. The maintainer retains final responsibility for scope, security,
releases, and accepting changes. A CLA, DCO, formal voting process, and public
roadmap are not required at this stage.

The repository exposes:

- `README.md` for positioning, installation, first use, and navigation.
- `CONTRIBUTING.md` for development setup, tests, change expectations, and the
  pull-request process.
- `CODE_OF_CONDUCT.md` for community behavior and enforcement.
- `SECURITY.md` for supported versions and private vulnerability reporting.
- `SUPPORT.md` for supported help channels and response expectations.
- `.github/` templates for actionable issues and pull requests.
- `docs/` for task-focused operator documentation.
- `docs/specs/product.md` as the only specification document.

Documentation must use repository-relative links, avoid promises the project
cannot support, and keep security-sensitive reports out of public issues.
Generated artifacts, internal planning notes, duplicate specifications, and
tool-specific working documents do not belong in the public documentation
tree.

## Documentation acceptance criteria

- Exactly one file exists under `docs/specs/`: this document.
- No legacy `docs/superpowers/` content remains.
- The README links the community policies, operational guides, and canonical
  product specification.
- Issue forms collect reproduction details and direct security reports to the
  private process.
- Pull requests describe motivation, validation, user-facing effects, and
  documentation impact.
- All repository-local Markdown links resolve.
- Documented commands and supported platforms match the implementation and CI.
