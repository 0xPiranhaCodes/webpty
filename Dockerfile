# syntax=docker/dockerfile:1
#
# webpty server image: one static Go binary with the web UI embedded,
# running as an unprivileged user on a small Alpine base.
#
#   docker build -t webpty .                    # builds the UI and binary from source
#   docker run --rm -p 127.0.0.1:8000:8000 -v webpty-data:/data webpty
#
#   scripts/image-binaries.sh dist dist/image   # release: binaries from verified archives
#   docker build --target release --build-context binaries=dist/image -t webpty .
#
# Base images are pinned by digest; update the tag and digest together.

FROM node:22.23.2-alpine3.23@sha256:72c5815a06aed9a2273aea5628d74d348af57843a7b547af2fe53dd3e4b95261 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.26.8-alpine3.23@sha256:a8fa79c5bd40d880b52bd3b6d7669ecdcfd00e85facdd427d279efb5ddd79cb1 AS build
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=web /src/web/dist web/dist
RUN go run ./internal/webassets/syncdist web/dist internal/webassets/dist \
	&& pkg=github.com/0xPiranhaCodes/webpty/internal/buildinfo \
	&& go build -trimpath \
		-ldflags "-s -w -X $pkg.version=${VERSION} -X $pkg.commit=${COMMIT} -X $pkg.date=${BUILD_DATE}" \
		-o /out/webpty ./cmd/webpty

FROM alpine:3.23.5@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40 AS runtime
# Nothing is installed, so the runtime is exactly the pinned base. /bin/sh
# comes from BusyBox; terminals run it unless WEBPTY_COMMAND names another
# shell installed in a derived image.
RUN addgroup -S -g 10001 webpty \
	&& adduser -S -D -H -u 10001 -G webpty -h /data -s /bin/sh webpty \
	&& mkdir -p /data \
	&& chown 10001:10001 /data \
	&& chmod 700 /data

# The container listens on every interface so the published port works;
# WEBPTY_PUBLIC_ORIGIN must be the URL browsers use. The default suits
# "docker run -p 127.0.0.1:8000:8000"; set https://your.host behind a TLS
# reverse proxy.
ENV WEBPTY_ADDRESS=0.0.0.0:8000 \
	WEBPTY_PUBLIC_ORIGIN=http://localhost:8000 \
	WEBPTY_DATABASE_PATH=/data/webpty.db \
	HOME=/data \
	SHELL=/bin/sh

VOLUME ["/data"]
EXPOSE 8000
USER 10001:10001
WORKDIR /data
# Probes 127.0.0.1 on the port of WEBPTY_ADDRESS, so changing the port
# through that variable keeps the check working. Binding one non-loopback
# address, or passing --port, needs a matching --health-cmd.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
	CMD ["/bin/sh", "-c", "wget -q -O /dev/null \"http://127.0.0.1:${WEBPTY_ADDRESS##*:}/healthz\""]
ENTRYPOINT ["/usr/local/bin/webpty"]
CMD ["serve"]

# Release images package the binaries from the verified release archives
# (named build context "binaries", laid out by scripts/image-binaries.sh).
FROM runtime AS release
ARG TARGETARCH
# hadolint ignore=DL3022
COPY --from=binaries linux_${TARGETARCH}/webpty /usr/local/bin/webpty

FROM runtime AS source
COPY --from=build /out/webpty /usr/local/bin/webpty
