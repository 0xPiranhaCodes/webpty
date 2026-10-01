# Contributing to webpty

Thanks for helping improve webpty. This is a maintainer-led project: maintainers
set project direction and make final decisions on scope, design, and whether a
change is accepted.

## Prerequisites

- Go 1.26.8 (the toolchain declared in `go.mod`; newer Go versions can download
  it automatically)
- Node.js 22.23.2 with npm
- Git and curl
- shellcheck and hadolint for `make check`
- Chromium and WebKit for `make e2e` (install once with
  `npx playwright install chromium webkit`)

## Set up the project

1. Fork and clone the repository.
2. Install the web dependencies:

   ```sh
   make web-install
   ```

3. Build the application:

   ```sh
   make build
   ```

## Make a focused change

Open an issue before starting a large change so its scope and approach can be
discussed. Keep each pull request focused on one concern, avoid unrelated
refactoring, and add or update tests for behavior changes. Preserve webpty's
security defaults and call out any security or privacy implications in the
pull request.

## Validate your change

Run the same primary checks used by continuous integration:

```sh
make check
make e2e
```

`make check` runs Go vet and race tests, cross-platform builds, frontend lint,
type checks, tests and builds, vulnerability and secret scans, and release
linting. If hadolint is unavailable locally, use the Docker override documented
in the README. `make e2e` runs the Chromium and WebKit end-to-end and
accessibility suites.

If a required check cannot run in your environment, state which command was
not run and why in the pull request.

## Documentation

Update user-facing documentation whenever behavior, configuration, commands,
security guidance, or supported workflows change. Keep examples runnable and
use the exact names and defaults implemented by the project.

## Commits and review

Use conventional commit messages. Examples:

```text
fix: reject expired share links
feat: add recording retention setting
docs: clarify reverse proxy setup
test: cover viewer disconnect on revoke
```

Maintainers review contributions as availability permits. They may request
changes, reshape the scope, or decline work that does not fit the project's
direction. A review or discussion does not guarantee that a pull request will
be merged.
