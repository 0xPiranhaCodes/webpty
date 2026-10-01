#!/bin/sh
# Extracts the Linux webpty binaries from verified release archives for the
# Dockerfile's release target, so the image ships exactly the binaries the
# archives, checksums, and provenance describe.
#
#   scripts/image-binaries.sh DIST_DIR OUT_DIR
#   docker build --target release --build-context binaries=OUT_DIR .
set -eu

die() {
	printf 'image-binaries: error: %s\n' "$*" >&2
	exit 1
}

[ $# -eq 2 ] || die "usage: image-binaries.sh DIST_DIR OUT_DIR"
dist=$1
out=$2
[ ! -e "$out" ] || die "$out already exists"
set -- "$dist"/webpty_*_checksums.txt
[ $# -eq 1 ] && [ -f "$1" ] || die "expected one checksums file in $dist"
checksums=$1
version=$(basename "$checksums")
version=${version#webpty_}
version=${version%_checksums.txt}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

staging=$(mktemp -d "${TMPDIR:-/tmp}/webpty-image.XXXXXX")
trap 'rm -rf "$staging"' EXIT
for arch in amd64 arm64; do
	archive=webpty_${version}_linux_${arch}.tar.gz
	[ -f "$dist/$archive" ] || die "$dist has no $archive"
	expected=$(awk -v name="$archive" '$2 == name {print $1}' "$checksums")
	[ -n "$expected" ] || die "$checksums has no checksum for $archive"
	[ "$(sha256 "$dist/$archive")" = "$expected" ] || die "checksum mismatch for $archive"
	mkdir "$staging/linux_$arch"
	tar -xzf "$dist/$archive" -C "$staging/linux_$arch" webpty
	[ -f "$staging/linux_$arch/webpty" ] && [ ! -L "$staging/linux_$arch/webpty" ] ||
		die "$archive has no regular webpty file"
	chmod 0755 "$staging/linux_$arch/webpty"
done
mkdir -p "$(dirname "$out")"
mv "$staging" "$out"
trap - EXIT
echo "image-binaries: ok: linux/amd64 and linux/arm64 binaries for webpty $version in $out"
