#!/bin/sh
# Prints a Homebrew formula for a webpty release, pinning every archive to
# the SHA-256 in that release's checksums file.
#
#   scripts/homebrew-formula.sh 1.2.3 dist/webpty_1.2.3_checksums.txt > webpty.rb
#   scripts/homebrew-formula.sh --local-dir "$PWD/dist" VERSION CHECKSUMS
#
# --local-dir points the formula at archives on this machine (file:// URLs)
# so a snapshot can be installed and tested before anything is published.
set -eu

repository=${WEBPTY_REPOSITORY:-0xPiranhaCodes/webpty}
local_dir=

die() {
	printf 'homebrew-formula: error: %s\n' "$*" >&2
	exit 1
}

if [ "${1:-}" = --local-dir ]; then
	[ $# -ge 2 ] || die "--local-dir needs a directory"
	local_dir=${2%/}
	shift 2
	case $local_dir in
	/*) ;;
	*) die "--local-dir must be absolute: $local_dir" ;;
	esac
fi
[ $# -eq 2 ] || die "usage: homebrew-formula.sh [--local-dir DIR] VERSION CHECKSUMS_FILE"
version=${1#v}
checksums=$2

printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' ||
	die "invalid version: $1"
[ -f "$checksums" ] || die "no checksums file at $checksums"

if [ -n "$local_dir" ]; then
	base=file://$local_dir
else
	base=https://github.com/$repository/releases/download/v$version
fi

sha() {
	name=webpty_${version}_$1.tar.gz
	value=$(awk -v name="$name" '$2 == name || $2 == "*" name {print $1}' "$checksums")
	printf '%s\n' "$value" | grep -Eq '^[0-9a-f]{64}$' || die "$checksums has no SHA-256 for $name"
	printf '%s\n' "$value"
}
darwin_arm64=$(sha darwin_arm64)
darwin_amd64=$(sha darwin_amd64)
linux_arm64=$(sha linux_arm64)
linux_amd64=$(sha linux_amd64)

cat <<EOF
class Webpty < Formula
  desc "Browser terminals with sharing, recording, and playback"
  homepage "https://github.com/$repository"
  version "$version"
  license "MIT"

  on_macos do
    on_arm do
      url "$base/webpty_${version}_darwin_arm64.tar.gz"
      sha256 "$darwin_arm64"
    end
    on_intel do
      url "$base/webpty_${version}_darwin_amd64.tar.gz"
      sha256 "$darwin_amd64"
    end
  end

  on_linux do
    on_arm do
      url "$base/webpty_${version}_linux_arm64.tar.gz"
      sha256 "$linux_arm64"
    end
    on_intel do
      url "$base/webpty_${version}_linux_amd64.tar.gz"
      sha256 "$linux_amd64"
    end
  end

  def install
    bin.install "webpty"
  end

  test do
    assert_match "webpty #{version}", shell_output("#{bin}/webpty version")
  end
end
EOF
