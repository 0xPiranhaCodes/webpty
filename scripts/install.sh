#!/bin/sh
# Installs a webpty release for macOS or Linux (amd64, arm64).
#
#   curl -fsSL https://raw.githubusercontent.com/0xPiranhaCodes/webpty/main/scripts/install.sh | sh
#   sh install.sh --version 1.2.3 --bin-dir "$HOME/bin"
#
# The archive is downloaded over HTTPS and installed only if its SHA-256
# matches the release's published checksums. Root is never required: the
# default destination is $HOME/.local/bin.
set -eu

releases_url=${WEBPTY_RELEASES_URL:-https://github.com/0xPiranhaCodes/webpty/releases}
version=${WEBPTY_VERSION:-latest}
bin_dir=${WEBPTY_BIN_DIR:-}

say() { printf 'webpty-install: %s\n' "$*"; }
die() {
	printf 'webpty-install: error: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Usage: install.sh [--version VERSION] [--bin-dir DIR]

  --version VERSION  release to install, e.g. 1.2.3 (default: latest)
  --bin-dir DIR      absolute directory for the webpty binary
                     (default: $HOME/.local/bin)

Environment: WEBPTY_VERSION, WEBPTY_BIN_DIR, WEBPTY_RELEASES_URL.
EOF
}

while [ $# -gt 0 ]; do
	case $1 in
	--version)
		[ $# -ge 2 ] || die "--version needs a value"
		version=$2
		shift 2
		;;
	--version=*)
		version=${1#--version=}
		shift
		;;
	--bin-dir)
		[ $# -ge 2 ] || die "--bin-dir needs a value"
		bin_dir=$2
		shift 2
		;;
	--bin-dir=*)
		bin_dir=${1#--bin-dir=}
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

# --- platform -------------------------------------------------------------

kernel=$(uname -s)
machine=$(uname -m)
case $kernel in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) die "unsupported operating system $kernel: webpty supports macOS and Linux (on Windows, use WSL or Docker)" ;;
esac
case $machine in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) die "unsupported architecture $machine: webpty supports amd64 and arm64" ;;
esac

# --- inputs ---------------------------------------------------------------

case $releases_url in
https://*) ;;
*) die "release URL must use https: $releases_url" ;;
esac
releases_url=${releases_url%/}

valid_version() {
	printf '%s\n' "$1" | grep -Eq '^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
}
if [ "$version" != latest ] && ! valid_version "$version"; then
	die "invalid version '$version': use latest or a release such as 1.2.3"
fi

[ -n "$bin_dir" ] || {
	[ -n "${HOME:-}" ] || die "HOME is not set; pass --bin-dir"
	bin_dir=$HOME/.local/bin
}
case $bin_dir in
/*) ;;
*) die "--bin-dir must be an absolute path: $bin_dir" ;;
esac
case /$bin_dir/ in
*/../* | */./*) die "--bin-dir must not contain . or .. components: $bin_dir" ;;
esac

# unsafe_dir succeeds when other users could replace what is in dir: it is
# owned by someone other than the installing user or root, or writable by
# everyone. A sticky world-writable ancestor such as /tmp is acceptable, but
# the destination itself never is. A symbolic link is judged by the
# directory it points to, and must itself belong to the user or root.
trusted_owner() {
	[ "$1" = 0 ] || [ "$1" = "$(id -u)" ]
}
unsafe_dir() {
	# shellcheck disable=SC2012 # ls -ld is the portable way to read mode and owner
	if [ -L "$1" ]; then
		trusted_owner "$(ls -ldn "$1" | awk '{print $3}')" || return 0
	fi
	# shellcheck disable=SC2012
	listing=$(ls -ldnL "$1" | awk '{print $1, $3}')
	mode=${listing% *}
	owner=${listing#* }
	trusted_owner "$owner" || return 0
	case $2:$mode in
	destination:????????w*) return 0 ;;
	ancestor:????????w[!tT]*) return 0 ;;
	esac
	return 1
}

# Every existing directory on the path must be safe; missing ones are
# created below the deepest existing one.
check_destination() {
	dir=$1
	role=destination
	while :; do
		if [ -e "$dir" ] || [ -L "$dir" ]; then
			if [ "$dir" = "$bin_dir" ] && [ -L "$dir" ]; then
				real=$(cd -P "$dir" 2>/dev/null && pwd -P) || die "$dir is a symbolic link to nothing; pass a real directory as --bin-dir"
				die "$dir is a symbolic link; the installer will not install through it. Pass the directory itself: --bin-dir $real"
			fi
			[ -d "$dir" ] || die "$dir exists and is not a directory"
			! unsafe_dir "$dir" "$role" || die "$dir can be modified by other users; choose a private --bin-dir"
			role=ancestor
		fi
		[ "$dir" = / ] && break
		dir=$(dirname "$dir")
	done
	if [ -L "$bin_dir/webpty" ]; then
		die "$bin_dir/webpty is a symbolic link; the installer will not replace it or its target. Remove it, or choose another --bin-dir"
	fi
	if [ -e "$bin_dir/webpty" ] && [ ! -f "$bin_dir/webpty" ]; then
		die "$bin_dir/webpty exists and is not a regular file; remove it first"
	fi
}
check_destination "$bin_dir"

# --- download -------------------------------------------------------------

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar >/dev/null 2>&1 || die "tar is required"
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
	die "sha256sum or shasum is required to verify the download"
fi

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
tag=v${version#v}
number=${version#v}

archive=webpty_${number}_${os}_${arch}.tar.gz
checksums=webpty_${number}_checksums.txt
base=$releases_url/download/$tag

work=$(mktemp -d "${TMPDIR:-/tmp}/webpty-install.XXXXXX")
staged=
cleanup() {
	rm -rf "$work"
	if [ -n "$staged" ]; then rm -f "$staged"; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

say "downloading webpty $tag for $os/$arch"
fetch --output "$work/$checksums" "$base/$checksums" || die "could not download $base/$checksums"
fetch --output "$work/$archive" "$base/$archive" || die "could not download $base/$archive"

expected=$(awk -v name="$archive" '$2 == name || $2 == "*" name {print $1}' "$work/$checksums")
[ -n "$expected" ] || die "$checksums has no checksum for $archive; refusing to install"
actual=$(sha256 "$work/$archive")
[ "$expected" = "$actual" ] || die "checksum mismatch for $archive (expected $expected, got $actual); refusing to install"
say "verified SHA-256 $actual"

mkdir "$work/x"
tar -xzf "$work/$archive" -C "$work/x" webpty || die "$archive does not contain webpty"
{ [ -f "$work/x/webpty" ] && [ ! -L "$work/x/webpty" ]; } || die "$archive has no regular webpty file"

# --- install --------------------------------------------------------------

mkdir -p "$bin_dir" || die "cannot create $bin_dir; choose a writable --bin-dir (root is not required)"
# Directories just created, or changed since the first check, are checked again.
check_destination "$bin_dir"
[ -w "$bin_dir" ] || die "$bin_dir is not writable; choose a writable --bin-dir (root is not required)"
staged=$bin_dir/.webpty.install.$$
cp "$work/x/webpty" "$staged"
chmod 0755 "$staged"
mv -f "$staged" "$bin_dir/webpty" || die "could not move the new binary into $bin_dir/webpty"
staged=

say "installed $bin_dir/webpty"
"$bin_dir/webpty" version
case :${PATH:-}: in
*:"$bin_dir":*) ;;
*) say "$bin_dir is not on your PATH; add it, for example: export PATH=\"$bin_dir:\$PATH\"" ;;
esac
