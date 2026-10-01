#!/bin/sh
# Completes a goreleaser dist/ directory before anything is signed: renders
# the Homebrew formula from the archives' checksums and lists webpty.rb in
# that same checksums file, so one signature and one provenance attestation
# cover the archives, the SBOMs, and the formula.
#
#   scripts/release-assemble.sh [DIST_DIR]
#
# WEBPTY_REPOSITORY (default 0xPiranhaCodes/webpty) names where the
# formula downloads archives from.
set -eu

die() {
	printf 'release-assemble: error: %s\n' "$*" >&2
	exit 1
}

dist=${1:-dist}
set -- "$dist"/webpty_*_checksums.txt
[ $# -eq 1 ] && [ -f "$1" ] || die "expected one checksums file in $dist"
checksums=$1
version=$(basename "$checksums")
version=${version#webpty_}
version=${version%_checksums.txt}

if [ -e "$dist/webpty.rb" ] || awk '$2 == "webpty.rb" {found = 1} END {exit !found}' "$checksums"; then
	die "$dist is already assembled (webpty.rb exists or is listed in $checksums)"
fi

formula=$dist/.webpty.rb.$$
trap 'rm -f "$formula"' EXIT
"$(dirname "$0")/homebrew-formula.sh" "$version" "$checksums" >"$formula"
if command -v ruby >/dev/null 2>&1; then
	ruby -c "$formula" >/dev/null || die "the rendered formula is not valid Ruby"
fi
if command -v sha256sum >/dev/null 2>&1; then
	sum=$(sha256sum "$formula" | awk '{print $1}')
else
	sum=$(shasum -a 256 "$formula" | awk '{print $1}')
fi
mv "$formula" "$dist/webpty.rb"
printf '%s  webpty.rb\n' "$sum" >>"$checksums"
echo "release-assemble: ok: webpty.rb ($sum) for webpty $version listed in $(basename "$checksums")"
