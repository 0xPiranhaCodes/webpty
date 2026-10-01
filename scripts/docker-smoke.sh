#!/bin/sh
# Runs a built webpty image and checks that it is healthy, unprivileged,
# keeps its database on the /data volume, and has a usable /bin/sh.
#
#   scripts/docker-smoke.sh webpty:ci
set -eu

image=${1:?usage: docker-smoke.sh IMAGE}
name=webpty-smoke-$$
volume=webpty-smoke-$$
die() {
	printf 'docker-smoke: FAIL: %s\n' "$*" >&2
	docker logs "$name" >&2 2>&1 || true
	exit 1
}
ok() { printf 'docker-smoke: ok: %s\n' "$*"; }
cleanup() {
	docker rm -f "$name" >/dev/null 2>&1 || true
	docker volume rm "$volume" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker run -d --name "$name" -p 127.0.0.1::8000 -v "$volume:/data" "$image" >/dev/null
port=$(docker port "$name" 8000/tcp | head -1 | sed 's/.*://')

i=0
until [ "$(docker inspect -f '{{.State.Health.Status}}' "$name")" = healthy ]; do
	i=$((i + 1))
	[ $i -lt 60 ] || die "container never became healthy"
	sleep 1
done
ok "health check reports healthy"

curl -fsS "http://127.0.0.1:$port/healthz" | grep -q '"ok"' || die "/healthz through the published port"
curl -fsS "http://localhost:$port/" | grep -q '/assets/' || die "embedded UI through the published port"
ok "published port serves /healthz and the UI"

[ "$(docker exec "$name" id -u)" = 10001 ] || die "container does not run as uid 10001"
[ "$(docker exec "$name" /bin/sh -c 'echo shell-ok')" = shell-ok ] || die "/bin/sh is not usable"
docker exec "$name" test -f /data/webpty.db || die "database is not on the /data volume"
docker exec "$name" webpty version
ok "runs as uid 10001 with /bin/sh; database on /data"

docker stop -t 20 "$name" >/dev/null
[ "$(docker inspect -f '{{.State.ExitCode}}' "$name")" = 0 ] || die "non-zero exit after docker stop"
ok "graceful stop"

# The health check follows the port set through WEBPTY_ADDRESS.
docker rm -f "$name" >/dev/null
docker run -d --name "$name" -e WEBPTY_ADDRESS=0.0.0.0:9000 -e WEBPTY_PUBLIC_ORIGIN=http://localhost:9000 \
	-v "$volume:/data" "$image" >/dev/null
i=0
until [ "$(docker inspect -f '{{.State.Health.Status}}' "$name")" = healthy ]; do
	i=$((i + 1))
	[ $i -lt 60 ] || die "container on port 9000 never became healthy"
	sleep 1
done
ok "health check follows WEBPTY_ADDRESS (port 9000)"
