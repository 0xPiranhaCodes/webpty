# Test and verification entry points. CI runs these same targets.
#
#   make check        everything except the browser suites
#   make e2e          browser end-to-end and accessibility suites (Chromium and WebKit)
#   make build        bin/webpty with the web UI embedded
#   make snapshot     local release archives in dist/, verified; nothing published
#
# Prerequisites: Go (go.mod's toolchain), Node.js 22.23.2 with npm, git, curl.
# make check also needs shellcheck and hadolint on PATH (or override, e.g.
# HADOLINT="docker run --rm -i hadolint/hadolint hadolint -" HADOLINT_INPUT="< Dockerfile").
# make snapshot needs syft; make e2e needs "npx playwright install chromium webkit";
# make homebrew-validate needs Homebrew; the docker targets need a Docker daemon.

GO ?= go
NPM ?= npm
NODE_VERSION ?= 22.23.2
GOVULNCHECK_VERSION ?= v1.8.0
GITLEAKS_VERSION ?= v8.30.1
GORELEASER_VERSION ?= v2.18.2
ACTIONLINT_VERSION ?= v1.7.12
# goreleaser needs a newer Go than go.mod pins. "go run" would hand that
# toolchain to the builds goreleaser starts, so goreleaser is installed as a
# binary and the release builds use go.mod's toolchain, as in release.yml.
TOOLS := $(CURDIR)/bin/tools
GORELEASER ?= $(TOOLS)/goreleaser-$(GORELEASER_VERSION)
ACTIONLINT ?= $(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)
SHELLCHECK ?= shellcheck
HADOLINT ?= hadolint
HADOLINT_INPUT ?= Dockerfile
DOCKER ?= docker
IMAGE ?= webpty:dev
BUILDINFO := github.com/0xPiranhaCodes/webpty/internal/buildinfo
VERSION ?= $(shell git describe --tags --match 'v[0-9]*' --dirty 2>/dev/null | sed 's/^v//' || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell git log -1 --format=%cI 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X $(BUILDINFO).version=$(or $(VERSION),dev) -X $(BUILDINFO).commit=$(COMMIT) -X $(BUILDINFO).date=$(BUILD_DATE)
SCRIPTS := scripts/install.sh scripts/homebrew-formula.sh scripts/homebrew-tap.sh scripts/homebrew-validate.sh \
	scripts/release-assemble.sh scripts/image-binaries.sh scripts/verify-archives.sh scripts/docker-smoke.sh
RACE_COUNT ?= 5
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64
VET_OSES := darwin linux
CONCURRENCY_PACKAGES := ./internal/app/... ./internal/session/... ./internal/httpapi/... ./internal/collab/... ./internal/access/... ./internal/recording/...
INTEGRATION_PACKAGES := ./internal/app/... ./internal/httpapi/... ./internal/store/... ./internal/recording/...

.PHONY: check test test-unit test-integration test-race test-race-repeat vet build-cross \
	web-install web-check e2e e2e-chromium e2e-webkit a11y vuln vuln-go vuln-npm secrets \
	web-assets build snapshot verify-archives homebrew-validate release-lint docker-build docker-smoke \
	image-binaries docker-release-smoke

check: vet test-race test-race-repeat build-cross web-check vuln secrets release-lint

test: test-unit test-integration

## Go unit tests and frontend unit tests.
test-unit:
	$(GO) test ./...
	cd web && $(NPM) test

## Go tests that run the HTTP API, store, and recorder against real SQLite and PTYs.
test-integration:
	$(GO) test -count=1 $(INTEGRATION_PACKAGES)

test-race:
	$(GO) test -race ./...

## The packages with goroutine-heavy code, repeated to shake out interleavings.
test-race-repeat:
	$(GO) test -race -count=$(RACE_COUNT) $(CONCURRENCY_PACKAGES)

vet:
	@set -e; for os in $(VET_OSES); do echo "GOOS=$$os go vet ./..."; GOOS=$$os $(GO) vet ./...; done

## CGO-free release builds for every supported platform.
build-cross:
	@set -e; for p in $(PLATFORMS); do \
		echo "CGO_ENABLED=0 GOOS=$${p%/*} GOARCH=$${p#*/} go build ./cmd/webpty"; \
		CGO_ENABLED=0 GOOS=$${p%/*} GOARCH=$${p#*/} $(GO) build -o /dev/null ./cmd/webpty; \
	done

web-install:
	cd web && $(NPM) ci

web-check:
	cd web && $(NPM) run lint -- --max-warnings=0
	cd web && $(NPM) run typecheck
	cd web && $(NPM) test
	cd web && $(NPM) run build

e2e:
	cd web && npx playwright test

e2e-chromium:
	cd web && npx playwright test --project chromium

e2e-webkit:
	cd web && npx playwright test --project webkit

## axe WCAG checks, keyboard-only traversal, focus visibility, and reduced motion.
a11y:
	cd web && npx playwright test e2e/pages-accessibility.spec.ts e2e/keyboard.spec.ts

vuln: vuln-go vuln-npm

vuln-go:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

vuln-npm:
	cd web && $(NPM) audit --audit-level=low

## Scans the full git history for committed secrets.
secrets:
	$(GO) run github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION) git --redact --no-banner .

## Builds the web UI and embeds it; every release binary needs this first.
## Refuses any Node.js but NODE_VERSION, which release.yml and the Dockerfile use.
web-assets:
	@v=$$(node --version); [ "$$v" = "v$(NODE_VERSION)" ] || { echo "web-assets: Node.js $$v; release assets are built with v$(NODE_VERSION)" >&2; exit 1; }
	cd web && $(NPM) ci --no-audit --no-fund && $(NPM) run build
	$(GO) run ./internal/webassets/syncdist web/dist internal/webassets/dist

build: web-assets
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/webpty ./cmd/webpty

## Release archives, checksums, SBOMs, and the Homebrew formula for every
## platform, assembled exactly as release.yml does, then the archive and
## clean-install checks. Signing and attestation need GitHub OIDC, so only the
## release workflow does them. Needs syft on PATH for the SBOMs.
snapshot: web-assets $(GORELEASER)
	$(GORELEASER) check
	$(GORELEASER) release --snapshot --clean --skip=publish
	scripts/release-assemble.sh dist
	$(MAKE) verify-archives

$(TOOLS)/goreleaser-$(GORELEASER_VERSION):
	mkdir -p $(TOOLS)
	GOBIN=$(TOOLS) $(GO) install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)
	mv $(TOOLS)/goreleaser $@

verify-archives:
	scripts/verify-archives.sh dist

## Installs the snapshot through a temporary local Homebrew tap (macOS/Linux with brew).
homebrew-validate:
	scripts/homebrew-validate.sh dist

release-lint:
	$(ACTIONLINT)
	$(SHELLCHECK) -s sh $(SCRIPTS)
	$(HADOLINT) $(HADOLINT_INPUT)

docker-build:
	$(DOCKER) build --build-arg VERSION=$(or $(VERSION),dev) --build-arg COMMIT=$(COMMIT) --build-arg BUILD_DATE=$(BUILD_DATE) -t $(IMAGE) .

docker-smoke: docker-build
	scripts/docker-smoke.sh $(IMAGE)

## The release image: the Dockerfile's release target packaging the Linux
## binaries from the verified archives in dist/ (run make snapshot first).
image-binaries:
	rm -rf dist/image
	scripts/image-binaries.sh dist dist/image

docker-release-smoke: image-binaries
	$(DOCKER) build --target release --build-context binaries=dist/image -t $(IMAGE) .
	scripts/docker-smoke.sh $(IMAGE)
