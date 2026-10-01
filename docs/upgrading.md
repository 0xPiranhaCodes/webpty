# Upgrading and verifying releases

## Upgrading

1. Read the release notes for the new version.
2. Back up: `webpty backup --output webpty-before-upgrade.db`
   ([backup-restore.md](backup-restore.md)).
3. Install the new binary (install script, Homebrew, archive, or image tag).
4. Run `webpty doctor` with your production environment. It reports how many
   database migrations the new version will apply.
5. Restart the server. Migrations run at startup, inside transactions, and
   are checksummed: webpty refuses to start on a database whose applied
   migrations were changed, or that was written by a newer version.

Downgrading is not supported once a newer version has migrated the
database. To go back, stop the server, install the older binary, and restore
the backup taken in step 2.

```sh
# install script: same command, optionally pinned
curl -fsSL https://raw.githubusercontent.com/0xPiranhaCodes/webpty/v1.3.0/scripts/install.sh | sh -s -- --version 1.3.0
# Homebrew: the helper from the new release's tag (see below)
curl -fsSLO https://raw.githubusercontent.com/0xPiranhaCodes/webpty/v1.3.0/scripts/homebrew-tap.sh
sh homebrew-tap.sh --version 1.3.0
# Docker
docker pull ghcr.io/0xpiranhacodes/webpty:1.3.0
```

`webpty version` prints the version, commit, and build date of the running
binary. A binary built from source without release flags reports version
`dev`.

## Homebrew

Homebrew installs formulae only from taps. There is no published tap, so
[`scripts/homebrew-tap.sh`](../scripts/homebrew-tap.sh) keeps one on your
machine, `webpty-local/webpty` (a plain directory under
`$(brew --repository)/Library/Taps`). For a release it:

1. downloads `webpty.rb`, `webpty_<version>_checksums.txt`, and its Sigstore
   bundle over HTTPS;
2. verifies the checksums' signature with cosign against the identity of the
   tagged release workflow (below), and refuses to continue otherwise;
3. checks that `webpty.rb` matches its SHA-256 in those checksums and is the
   formula for that version;
4. replaces `Formula/webpty.rb` in the local tap atomically;
5. on Homebrew 7 and later, trusts only that formula
   (`brew trust --formula webpty-local/webpty/webpty`). Homebrew ignores
   formulae from untrusted non-official taps;
6. runs `brew install --formula webpty-local/webpty/webpty`, or
   `brew upgrade --formula webpty-local/webpty/webpty` if it is already
   installed.

```sh
brew install cosign
curl -fsSLO https://raw.githubusercontent.com/0xPiranhaCodes/webpty/v1.3.0/scripts/homebrew-tap.sh
sh homebrew-tap.sh --version 1.3.0
```

The helper holds the signer identity it checks, so download it from the tag
of the release you install, never from a branch. Each release's notes print
these two commands for that tag; run them again for every upgrade.

`brew upgrade` on its own does not see new releases, because nothing updates
the local tap; run the helper instead. Homebrew then checks each archive
against the SHA-256 pinned in the formula. To remove everything:

```sh
brew uninstall webpty-local/webpty/webpty
brew untrust --formula webpty-local/webpty/webpty
brew untap webpty-local/webpty
```

`--skip-signature-verification` trusts the checksums as downloaded over
HTTPS, without cosign. Use it only if you verify the release some other way.

## Verifying a release

Every release publishes, for macOS and Linux on amd64 and arm64:

| File | Purpose |
| --- | --- |
| `webpty_<version>_<os>_<arch>.tar.gz` | The archive: `webpty`, `LICENSE`, `README.md`, `docs/` |
| `<archive>.spdx.json` | SPDX software bill of materials for each archive |
| `webpty.rb` | Homebrew formula pinned to the archives' checksums |
| `webpty_<version>_checksums.txt` | SHA-256 of every archive, SBOM, and `webpty.rb` |
| `webpty_<version>_checksums.txt.sigstore.json` | Keyless Sigstore signature of the checksums |

Archives are built reproducibly: CGO is off, paths are trimmed, and file
times are the tagged commit's time, so rebuilding a tag with the same
toolchain gives the same bytes.

**Checksums** (what the install script and the Homebrew helper check):

```sh
sha256sum --check --ignore-missing webpty_1.3.0_checksums.txt     # Linux
shasum -a 256 --check --ignore-missing webpty_1.3.0_checksums.txt # macOS
```

**Signature** of the checksums file. It is made by the release workflow
running for that exact tag, so the identity names both the workflow file
and the tag. A signature from any other workflow, branch, or tag of the
repository does not verify. This requires
[cosign](https://docs.sigstore.dev/cosign/system_config/installation/) 2.4 or
newer:

```sh
cosign verify-blob \
  --bundle webpty_1.3.0_checksums.txt.sigstore.json \
  --certificate-identity https://github.com/0xPiranhaCodes/webpty/.github/workflows/release.yml@refs/tags/v1.3.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  webpty_1.3.0_checksums.txt
```

**Build provenance** covers the archives, the checksums file, `webpty.rb`,
and the container image. Verify it with the
[GitHub CLI](https://cli.github.com/), pinned to the release workflow and tag:

```sh
gh attestation verify webpty_1.3.0_darwin_arm64.tar.gz --repo 0xPiranhaCodes/webpty \
  --signer-workflow 0xPiranhaCodes/webpty/.github/workflows/release.yml --source-ref refs/tags/v1.3.0
gh attestation verify webpty.rb --repo 0xPiranhaCodes/webpty \
  --signer-workflow 0xPiranhaCodes/webpty/.github/workflows/release.yml --source-ref refs/tags/v1.3.0
gh attestation verify oci://ghcr.io/0xpiranhacodes/webpty:1.3.0 --repo 0xPiranhaCodes/webpty \
  --signer-workflow 0xPiranhaCodes/webpty/.github/workflows/release.yml --source-ref refs/tags/v1.3.0
```

## How releases are made

Pushing a `vX.Y.Z` (or `vX.Y.Z-prerelease`) tag runs
`.github/workflows/release.yml`. The release stays a draft until every job
has passed:

1. **Read-only checks.** These jobs have no write permission and no OIDC
   token: the tag must be a release version; vet, race tests, and frontend
   checks; govulncheck, `npm audit`, and gitleaks.
2. **Web UI.** A read-only job builds it with npm and hands the built files
   on as an artifact. No later job runs npm.
3. **Archives** (read-only). goreleaser builds the four archives, checksums,
   and SBOMs from the embedded UI. `scripts/release-assemble.sh` renders
   `webpty.rb` and lists it in the checksums. `scripts/verify-archives.sh`
   then checks the contents, ownership, and checksums and does a clean
   install.
4. **Homebrew** (macOS, read-only). Installs, tests, and upgrades the formula
   through `homebrew-tap.sh`.
5. **Image build** (read-only). Builds the Dockerfile's `release` target for
   linux/amd64 and linux/arm64 from the verified Linux archives' binaries
   into a local OCI archive, records its image index digest, and runs
   `scripts/docker-smoke.sh` against that archive's linux/amd64 image.
6. **Image attestation** (OIDC and attestation permissions only, no
   registry access). Attests build provenance for that digest and verifies
   it against the tagged release workflow.
7. **Publish.** Re-checks every checksum, signs the checksums keylessly, and
   verifies the signature against the exact identity above. It attests
   provenance for the archives, checksums, and formula, then uploads
   everything to a **draft** release.
8. **Image.** The only job that can write to `ghcr.io`, and the first
   registry write of the release. It checks the archive's SHA-256 and
   verifies the digest's attestation again. Then it copies the archive
   unchanged (`--preserve-digests`) to `X.Y.Z`, plus `X.Y` and `latest` for
   releases without a prerelease suffix. Every tag must resolve to the
   attested digest.
9. **Finalize.** Only now is the release taken out of draft.

If any step fails, users see no release (only a draft, or nothing). No image
tag exists until the image is smoke-tested and its provenance is attested
and verified. Nothing is staged on the registry first, so a failed run
leaves nothing to clean up. Actions are pinned to commit SHAs, Go to
`go.mod`'s toolchain (1.26.8), Node.js to 22.23.2 (as in the Dockerfile),
and each job has only the permissions it needs.

To rehearse a release locally without publishing anything:

```sh
make snapshot              # web UI, then dist/: archives, checksums, SBOMs, webpty.rb; archive and clean-install checks
make homebrew-validate     # installs, tests, and upgrades the snapshot through a temporary local tap, then removes it
make docker-release-smoke  # release image from the snapshot archives: health, user, volume, shell, health port
make docker-smoke          # the same checks for an image built from source
```

`make snapshot` needs [syft](https://github.com/anchore/syft) on `PATH` for
SBOMs. Signing and attestation need GitHub's OIDC token, so only the release
workflow can do them.
