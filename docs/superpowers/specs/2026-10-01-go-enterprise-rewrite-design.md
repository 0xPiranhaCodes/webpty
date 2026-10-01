# webpty enterprise rewrite design

## Product boundary

webpty is a self-hosted terminal collaboration service. The Go process and all
PTY child processes run on the same macOS or Linux host; remote execution and a
multi-node control plane are outside this design.

The product has one administrator. A new installation starts with the password
`CHANGEME`, and the administrator must rotate it at first login before using any
other authenticated feature. A terminal workspace has one owner, may have at
most one invited editor, and may have multiple read-only viewers.

Sharing uses expiring, revocable capability links. A link's plaintext token is
shown only when it is created. Persistence contains a cryptographic token hash,
its scope, expiry, revocation state, and audit metadata, never the plaintext
token.

## Runtime architecture

The application is one Go binary built with exactly Go 1.26.8. `go.mod`
declares `go 1.26.0` as the language version and pins `toolchain go1.26.8`.
That toolchain line is the single source of truth: every workflow's
`setup-go` reads it, the Dockerfile's builder is `golang:1.26.8`, and the
release builds must report `go1.26.8` (`scripts/verify-archives.sh` checks
the shipped binary). The patch release is pinned, not just the minor,
because release archives must be byte-for-byte reproducible and must carry
that patch release's standard-library security fixes. GoReleaser itself
needs a newer Go. The Makefile therefore installs it as a separate binary,
so its toolchain never builds webpty. Changing the version means changing
`go.mod` and the Dockerfile together; `internal/repocheck` fails otherwise.
Standard-library `net/http` provides the
foundation and exposes a versioned `/api/v1` API, plus operational endpoints
such as `GET /healthz`. Application wiring lives in `internal/app`, environment
configuration in `internal/config`, and HTTP transport code in
`internal/httpapi`.

SQLite is the system of record for users, sessions, workspace authorization,
capabilities, terminal metadata, recordings, and audit events. Schema changes
will use ordered migrations and transactions. The deployment owns its database
file and must back it up together with any recording data.

The React and TypeScript client uses a precision-dark visual system and is built
with Vite. Production assets are embedded from `internal/webassets/dist` so the
server can be distributed as one binary without CDN dependencies. During
development, Vite can serve the client separately. Run
`go generate ./internal/webassets` before a production Go build to install,
build, and copy the current frontend bundle into the embed directory.

## Terminal and recording model

The server owns PTY creation, process lifecycle, dimensions, and fan-out. It
records terminal output, resize events, lifecycle events, and participant
presence with ordered timestamps sufficient for replay. It never records exact
keyboard input. Input may be forwarded to the PTY for an authorized owner or
editor, but keystrokes and input payloads must not enter recordings, logs,
analytics, or audit details.

The owner retains control of a workspace. The single editor slot grants input
permission while viewer access remains read-only. Disconnects update presence
without implicitly terminating the underlying terminal; explicit lifecycle
rules govern process termination and replay completion.

## Security constraints

- Passwords use a memory-hard password hash; `CHANGEME` is never accepted after
  the mandatory rotation has completed.
- Authorization is enforced server-side for every API and terminal connection.
- Capability validation hashes the presented token and checks scope, expiry,
  and revocation before granting access.
- Sensitive values are excluded from logs and errors. Responses use secure
  cookie, origin, and transport settings appropriate to self-hosted deployment.
- SQLite constraints and transactions enforce the one-owner/one-editor model.

## Delivery constraints

The legacy Python implementation remains intact during the rewrite. Go and
React code are introduced alongside it until an explicit migration task changes
the release path. CI verifies Go tests with the race detector and vet, and
verifies frontend linting, type safety, tests, and production builds on both
macOS and Linux.
