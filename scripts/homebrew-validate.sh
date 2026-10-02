#!/bin/sh
# Installs a local goreleaser build (dist/ by default) with Homebrew through
# the same helper users run (scripts/homebrew-tap.sh), runs brew test, runs
# the helper again to take the upgrade path, then removes the formula, its
# trust entry, and the temporary tap. Nothing is published and no remote tap
# is touched. Snapshots are unsigned, so the signature check is skipped; the
# formula's own checksum and every archive checksum are still verified.
#
#   scripts/homebrew-validate.sh [DIST_DIR]
set -eu

die() {
	printf 'homebrew-validate: error: %s\n' "$*" >&2
	exit 1
}

command -v brew >/dev/null 2>&1 || die "brew is not installed"
dist=$(cd "${1:-dist}" && pwd)
set -- "$dist"/webpty_*_checksums.txt
{ [ $# -eq 1 ] && [ -f "$1" ]; } || die "expected one checksums file in $dist"
checksums=$1
version=$(basename "$checksums")
version=${version#webpty_}
version=${version%_checksums.txt}

export HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ANALYTICS=1 HOMEBREW_NO_INSTALL_CLEANUP=1 HOMEBREW_NO_ENV_HINTS=1
tap=webpty-local/validate
formula=$tap/webpty
if brew list --formula webpty >/dev/null 2>&1 || brew list --formula "$formula" >/dev/null 2>&1; then
	die "a webpty formula is already installed; uninstall it first"
fi
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
	die "sha256sum or shasum is required"
fi
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/webpty-brew-validate.XXXXXX")
tap_dir=$(brew --repository "$tap")
[ ! -e "$tap_dir" ] || die "$tap_dir already exists; remove it first"
cleanup() {
	brew uninstall --formula "$formula" >/dev/null 2>&1 || true
	if brew commands --quiet 2>/dev/null | grep -qx untrust; then
		brew untrust --formula "$formula" >/dev/null 2>&1 || true
	fi
	rm -rf "$tap_dir" "$work"
	rmdir "$(dirname "$tap_dir")" 2>/dev/null || true
}
trap cleanup EXIT

# The same formula the release attaches, pointed at the local archives, and
# listed in a copy of the checksums the way release-assemble.sh lists it.
"$here/homebrew-formula.sh" --local-dir "$dist" "$version" "$checksums" >"$work/webpty.rb"
ruby -c "$work/webpty.rb" >/dev/null
grep -v '  webpty\.rb$' "$checksums" >"$work/$(basename "$checksums")" || true
printf '%s  webpty.rb\n' "$(sha256 "$work/webpty.rb")" >>"$work/$(basename "$checksums")"

sh "$here/homebrew-tap.sh" --from-dir "$work" --version "$version" --tap "$tap" --skip-signature-verification
brew test "$formula"
installed=$(brew --prefix "$formula")/bin/webpty
"$installed" version | grep -F "webpty $version" >/dev/null || die "brew installed $("$installed" version)"

expected=$(awk -v n="webpty_${version}_$(uname -s | tr '[:upper:]' '[:lower:]')_$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz" '$2 == n {print $1}' "$checksums")
cached=$(brew --cache --formula "$formula")
if [ -f "$cached" ]; then
	actual=$(sha256 "$cached")
	[ "$actual" = "$expected" ] || die "brew installed $actual, published $expected"
	echo "homebrew-validate: ok: brew downloaded the host archive with SHA-256 $actual"
fi

# Running the helper again is how users upgrade; with the same version it
# must be a clean no-op through brew upgrade.
sh "$here/homebrew-tap.sh" --from-dir "$work" --version "$version" --tap "$tap" --skip-signature-verification
echo "homebrew-validate: ok: webpty $version installs, passes brew test, and upgrades by formula name"
