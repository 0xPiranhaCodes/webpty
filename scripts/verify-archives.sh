#!/bin/sh
# Checks a release directory produced by goreleaser (dist/ by default):
#
#   1. every file the checksums list matches, every archive has an SBOM, and
#      webpty.rb is listed and pins every archive's checksum
#   2. every archive holds exactly webpty, LICENSE, README.md, and docs/*.md,
#      with webpty executable and nothing else
#   3. this host's archive installs into an empty directory and, with an empty
#      HOME and minimal PATH, reports its version, passes doctor, serves the
#      embedded UI, and forces the CHANGEME password to be changed
#
#   scripts/verify-archives.sh [DIST_DIR]
set -eu

dist=${1:-dist}
die() {
	printf 'verify-archives: FAIL: %s\n' "$*" >&2
	exit 1
}
ok() { printf 'verify-archives: ok: %s\n' "$*"; }

if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | awk '{print $1}'; }
else
	sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
fi

set -- "$dist"/webpty_*_checksums.txt
[ $# -eq 1 ] && [ -f "$1" ] || die "expected one webpty_*_checksums.txt in $dist"
checksums=$1
version=$(basename "$checksums")
version=${version#webpty_}
version=${version%_checksums.txt}

# --- 1. checksums and SBOMs ----------------------------------------------

count=0
for platform in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
	name=webpty_${version}_${platform}.tar.gz
	[ -f "$dist/$name" ] || die "missing $name"
	expected=$(awk -v n="$name" '$2 == n {print $1}' "$checksums")
	[ -n "$expected" ] || die "$name is not in $(basename "$checksums")"
	actual=$(sha256 "$dist/$name")
	[ "$expected" = "$actual" ] || die "$name: checksum $actual, published $expected"
	[ -s "$dist/$name.spdx.json" ] || die "missing SBOM $name.spdx.json"
	grep -q '"spdxVersion"' "$dist/$name.spdx.json" || die "$name.spdx.json is not an SPDX document"
	count=$((count + 1))
	ok "$name sha256 $actual, SBOM present"
done
listed=$(grep -c '\.tar\.gz$' "$checksums")
[ "$listed" -eq "$count" ] || die "checksums list $listed archives, expected $count"

# Every listed file, the formula included, matches.
listing=${checksums##*/}
while read -r expected name; do
	[ -f "$dist/$name" ] || die "$listing lists $name, which is missing"
	[ "$(sha256 "$dist/$name")" = "$expected" ] || die "$name does not match $listing"
done <"$checksums"
ok "all $(wc -l <"$checksums" | tr -d ' ') entries of $(basename "$checksums") match"

formula=$dist/webpty.rb
[ -f "$formula" ] || die "missing webpty.rb (scripts/release-assemble.sh adds it)"
awk '$2 == "webpty.rb"' "$checksums" | grep -q . || die "webpty.rb is not in $(basename "$checksums")"
grep -Fqx "  version \"$version\"" "$formula" || die "webpty.rb is not for version $version"
for platform in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
	name=webpty_${version}_${platform}.tar.gz
	sum=$(awk -v n="$name" '$2 == n {print $1}' "$checksums")
	if ! grep -Fq "/$name\"" "$formula" || ! grep -Fq "sha256 \"$sum\"" "$formula"; then
		die "webpty.rb does not pin $name to $sum"
	fi
done
ok "webpty.rb is listed in the checksums and pins all four archives"

# --- 2. contents ---------------------------------------------------------

expected_files=$(cd "$(dirname "$0")/.." && { printf '%s\n' webpty LICENSE README.md; for f in docs/*.md; do printf '%s\n' "$f"; done; } | LC_ALL=C sort)
for platform in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
	name=webpty_${version}_${platform}.tar.gz
	files=$(tar -tzf "$dist/$name" | LC_ALL=C sort)
	[ "$files" = "$expected_files" ] || die "$name holds:
$files
expected:
$expected_files"
	mode=$(tar -tzvf "$dist/$name" webpty | awk '{print $1}')
	[ "$mode" = "-rwxr-xr-x" ] || die "$name: webpty mode is $mode"
	foreign=$(tar -tzvf "$dist/$name" | grep -Ev 'root[/ ]+root' || true)
	[ -z "$foreign" ] || die "$name has entries not owned by root:root:
$foreign"
	ok "$name holds exactly $(printf '%s\n' "$files" | wc -l | tr -d ' ') expected files, webpty -rwxr-xr-x, all root:root"
	tar -tzvf "$dist/$name" | sed "s|^|    $name: |"
done

# --- 3. clean install on this host ---------------------------------------

case $(uname -s) in Darwin) os=darwin ;; Linux) os=linux ;; *) die "unsupported host" ;; esac
case $(uname -m) in x86_64 | amd64) arch=amd64 ;; arm64 | aarch64) arch=arm64 ;; *) die "unsupported host" ;; esac
archive=$dist/webpty_${version}_${os}_${arch}.tar.gz

root=$(mktemp -d "${TMPDIR:-/tmp}/webpty-clean-install.XXXXXX")
pid=
cleanup() {
	[ -z "$pid" ] || kill "$pid" 2>/dev/null || true
	rm -rf "$root"
}
trap cleanup EXIT
mkdir -p "$root/bin" "$root/home" "$root/data"
chmod 700 "$root/data"
tar -xzf "$archive" -C "$root/bin" webpty
run() { env -i HOME="$root/home" PATH=/usr/bin:/bin SHELL=/bin/sh WEBPTY_DATABASE_PATH="$root/data/webpty.db" "$@"; }

out=$(run "$root/bin/webpty" version)
case $out in "webpty $version (commit "*) ;; *) die "version printed: $out" ;; esac
# Release builds use go.mod's toolchain, as setup-go does in release.yml.
toolchain=$(awk '$1 == "toolchain" {print $2}' "$(dirname "$0")/../go.mod")
case $out in *", $toolchain, "*) ;; *) die "built with another Go than go.mod's $toolchain: $out" ;; esac
ok "clean install: $out"

port=$(awk 'BEGIN { srand(); print 20000 + int(rand() * 20000) }')
run "$root/bin/webpty" doctor -p "$port" >"$root/doctor.txt" || die "doctor failed:
$(cat "$root/doctor.txt")"
[ -z "$(ls -A "$root/data")" ] || die "doctor changed the data directory: $(ls -A "$root/data")"
ok "clean install: doctor $(tail -1 "$root/doctor.txt"); data directory untouched"

# Not through run(): a backgrounded function is a subshell, and $! must be
# webpty itself (env execs it).
env -i HOME="$root/home" PATH=/usr/bin:/bin SHELL=/bin/sh WEBPTY_DATABASE_PATH="$root/data/webpty.db" \
	"$root/bin/webpty" serve -p "$port" >"$root/serve.log" 2>&1 &
pid=$!
base=http://127.0.0.1:$port
i=0
until curl -fsS "$base/healthz" >/dev/null 2>&1; do
	i=$((i + 1))
	[ $i -lt 100 ] || die "server never became healthy: $(cat "$root/serve.log")"
	sleep 0.1
done
ok "clean install: /healthz $(curl -fsS "$base/healthz")"

index=$(curl -fsS "$base/")
asset=$(printf '%s\n' "$index" | sed -n 's/.*src="\(\/assets\/[^"]*\.js\)".*/\1/p' | head -1)
[ -n "$asset" ] || die "index.html references no script"
curl -fsS -o /dev/null "$base$asset" || die "embedded asset $asset not served"
curl -fsS "$base/admin/sessions/x" | grep -q "$asset" || die "SPA route is not served index.html"
ok "clean install: embedded UI and $asset served offline"

jar=$root/cookies
code=$(curl -s -o "$root/login.json" -w '%{http_code}' -c "$jar" -H 'Content-Type: application/json' -H "Origin: $base" \
	-d '{"password":"CHANGEME"}' "$base/api/v1/admin/login")
[ "$code" = 200 ] || die "first-run login returned $code"
curl -fsS -b "$jar" "$base/api/v1/admin/session" | grep -q '"passwordChangeRequired":true' ||
	die "first-run session does not require a password change"
ok "clean install: CHANGEME signs in and must be rotated"

kill -TERM "$pid"
wait "$pid" || die "server exited non-zero after SIGTERM: $(cat "$root/serve.log")"
pid=
[ -f "$root/data/webpty.db" ] || die "no database created"
ok "clean install: graceful SIGTERM shutdown; database at a private path"
ok "all checks passed for webpty $version"
