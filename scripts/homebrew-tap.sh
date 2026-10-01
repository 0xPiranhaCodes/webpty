#!/bin/sh
# Installs or upgrades webpty with Homebrew from a release's signed formula,
# through a tap that lives only on this machine. No remote tap is needed.
#
#   curl -fsSLO https://raw.githubusercontent.com/0xPiranhaCodes/webpty/v1.2.3/scripts/homebrew-tap.sh
#   sh homebrew-tap.sh --version 1.2.3    # the release whose tag you fetched it from
#
# Before brew sees the formula, its SHA-256 must match the release's
# checksums file, and that file's Sigstore signature must come from this
# repository's release workflow on the release tag (cosign is required).
# The formula then pins every archive's SHA-256, which brew checks.
set -eu

repository=${WEBPTY_REPOSITORY:-0xPiranhaCodes/webpty}
releases_url=${WEBPTY_RELEASES_URL:-https://github.com/$repository/releases}
version=${WEBPTY_VERSION:-latest}
tap=webpty-local/webpty
from_dir=
verify_signature=1
issuer=https://token.actions.githubusercontent.com

say() { printf 'webpty-homebrew: %s\n' "$*"; }
die() {
	printf 'webpty-homebrew: error: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Usage: homebrew-tap.sh [--version VERSION] [--tap USER/REPO] [--from-dir DIR]
                       [--skip-signature-verification]

  --version VERSION   release to install, e.g. 1.2.3 (default: latest)
  --tap USER/REPO     local tap that holds the formula (default: webpty-local/webpty)
  --from-dir DIR      read webpty.rb, the checksums, and the signature bundle
                      from DIR instead of downloading them (needs --version)
  --skip-signature-verification
                      trust the checksums over HTTPS alone (not recommended)

Environment: WEBPTY_VERSION, WEBPTY_RELEASES_URL, WEBPTY_REPOSITORY.
EOF
}

while [ $# -gt 0 ]; do
	case $1 in
	--version)
		[ $# -ge 2 ] || die "--version needs a value"
		version=$2
		shift 2
		;;
	--tap)
		[ $# -ge 2 ] || die "--tap needs a value"
		tap=$2
		shift 2
		;;
	--from-dir)
		[ $# -ge 2 ] || die "--from-dir needs a value"
		from_dir=$2
		shift 2
		;;
	--skip-signature-verification)
		verify_signature=
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage >&2
		die "unknown argument: $1"
		;;
	esac
done

valid_version() {
	printf '%s\n' "$1" | grep -Eq '^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
}
if [ "$version" != latest ] && ! valid_version "$version"; then
	die "invalid version '$version': use latest or a release such as 1.2.3"
fi
printf '%s\n' "$tap" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_-]*/[A-Za-z0-9][A-Za-z0-9_-]*$' ||
	die "invalid tap '$tap': use USER/REPO"
printf '%s\n' "$repository" | grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' ||
	die "invalid repository '$repository'"
if [ -n "$from_dir" ]; then
	case $from_dir in
	/*) ;;
	*) die "--from-dir must be an absolute path: $from_dir" ;;
	esac
	[ "$version" != latest ] || die "--from-dir needs --version"
	[ -d "$from_dir" ] || die "$from_dir is not a directory"
else
	case $releases_url in
	https://*) ;;
	*) die "release URL must use https: $releases_url" ;;
	esac
	releases_url=${releases_url%/}
fi

command -v brew >/dev/null 2>&1 || die "Homebrew (brew) is not installed"
if [ -n "$verify_signature" ]; then
	command -v cosign >/dev/null 2>&1 ||
		die "cosign is required to verify the release signature (brew install cosign), or pass --skip-signature-verification"
fi
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
	die "sha256sum or shasum is required"
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/webpty-homebrew.XXXXXX")
staged=
cleanup() {
	rm -rf "$work"
	if [ -n "$staged" ]; then rm -f "$staged"; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

fetch() {
	curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location --retry 2 "$@"
}

if [ "$version" = latest ]; then
	resolved=$(fetch --output /dev/null --write-out '%{url_effective}' "$releases_url/latest") ||
		die "could not find the latest release at $releases_url"
	version=${resolved##*/tag/}
	if [ "$version" = "$resolved" ] || ! valid_version "$version"; then
		die "the latest release resolved to an unexpected URL: $resolved"
	fi
fi
number=${version#v}
tag=v$number
checksums=webpty_${number}_checksums.txt
bundle=$checksums.sigstore.json

for file in webpty.rb "$checksums" "$bundle"; do
	if [ -z "$verify_signature" ] && [ "$file" = "$bundle" ]; then
		continue
	fi
	if [ -n "$from_dir" ]; then
		[ -f "$from_dir/$file" ] || die "$from_dir has no $file"
		cp "$from_dir/$file" "$work/$file"
	else
		fetch --output "$work/$file" "$releases_url/download/$tag/$file" ||
			die "could not download $releases_url/download/$tag/$file"
	fi
done

if [ -n "$verify_signature" ]; then
	identity=https://github.com/$repository/.github/workflows/release.yml@refs/tags/$tag
	cosign verify-blob --bundle "$work/$bundle" \
		--certificate-identity "$identity" \
		--certificate-oidc-issuer "$issuer" \
		"$work/$checksums" >/dev/null 2>&1 ||
		die "the signature of $checksums does not verify as $identity; refusing to install"
	say "verified the signature of $checksums ($identity)"
else
	say "warning: not verifying the release signature; trusting $checksums as downloaded"
fi

expected=$(awk '$2 == "webpty.rb" || $2 == "*webpty.rb" {print $1}' "$work/$checksums")
[ -n "$expected" ] || die "$checksums has no checksum for webpty.rb; refusing to install"
[ "$(printf '%s\n' "$expected" | wc -l)" -eq 1 ] || die "$checksums lists webpty.rb more than once; refusing to install"
actual=$(sha256 "$work/webpty.rb")
[ "$expected" = "$actual" ] || die "checksum mismatch for webpty.rb (expected $expected, got $actual); refusing to install"
grep -Fqx "  version \"$number\"" "$work/webpty.rb" || die "webpty.rb is not the formula for version $number"
say "verified webpty.rb SHA-256 $actual"

# The tap is a plain directory: brew tap-new would also switch Homebrew
# into developer mode.
tap_dir=$(brew --repository "$tap")
mkdir -p "$tap_dir/Formula"
staged=$tap_dir/Formula/.webpty.rb.$$
cp "$work/webpty.rb" "$staged"
chmod 0644 "$staged"
mv -f "$staged" "$tap_dir/Formula/webpty.rb"
staged=
say "placed the formula in the local tap $tap ($tap_dir)"

formula=$tap/webpty
export HOMEBREW_NO_AUTO_UPDATE=1
# Homebrew 7 loads formulae from non-official taps only once trusted;
# trust is scoped to this one formula rather than the whole tap.
if brew commands --quiet 2>/dev/null | grep -qx trust; then
	brew trust --formula "$formula"
fi
if brew list --formula "$formula" >/dev/null 2>&1; then
	brew upgrade --formula "$formula"
	say "webpty is up to date with $tag ($formula)"
else
	brew install --formula "$formula"
	say "installed webpty $tag ($formula); run this script again to upgrade"
fi
