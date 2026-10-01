# Deployment, TLS, and reverse proxies

A webpty terminal is a shell on the server, running as the user that runs
webpty. Treat access to webpty like SSH access to that account.

## The secure default

With no configuration webpty listens on `127.0.0.1:8000`, so only programs on
the same machine can reach it. That is the right setting for personal use on
a laptop or workstation, and for reaching a server through an SSH tunnel:

```sh
ssh -L 8000:127.0.0.1:8000 you@server   # then browse http://127.0.0.1:8000
```

## Exposing webpty to a network

To serve other machines:

1. Keep webpty on loopback (`WEBPTY_ADDRESS=127.0.0.1:8000`) and put a
   reverse proxy in front of it that terminates TLS.
2. Set `WEBPTY_PUBLIC_ORIGIN` to the exact URL browsers use, for example
   `https://pty.example.com`. Secure cookies turn on automatically for an
   `https` origin.
3. Run `webpty doctor` with the same environment; it fails or warns on
   incompatible combinations (a non-loopback bind without an origin, secure
   cookies on plain HTTP, an `https` origin with secure cookies off).

webpty refuses to start on a non-loopback address without
`WEBPTY_PUBLIC_ORIGIN`. Plain HTTP beyond loopback is possible but sends the
password, session cookies, and every keystroke unencrypted; don't.

The proxy must:

- pass WebSocket upgrades for `/api/v1/terminals/{id}/ws`;
- keep the `Origin` header (webpty rejects cross-origin requests);
- allow long-lived connections: terminals stay open for hours.

webpty does not trust `X-Forwarded-For`. Sign-in rate limits and audit
entries therefore see the proxy's address, so all clients share one limit.
Rate-limit sign-in at the proxy as well if many people use one instance.

### Caddy

```caddyfile
pty.example.com {
	reverse_proxy 127.0.0.1:8000
}
```

Caddy obtains a certificate and proxies WebSockets without further settings.

### nginx

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 443 ssl;
    http2 on;
    server_name pty.example.com;
    ssl_certificate     /etc/letsencrypt/live/pty.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/pty.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8000;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
        proxy_buffering off;
    }
}
```

## Running as a service

Create a dedicated user and a private data directory; terminals run as that
user, so give it only the access its terminals need.

### systemd (Linux)

```ini
# /etc/systemd/system/webpty.service
[Unit]
Description=webpty browser terminals
After=network-online.target
Wants=network-online.target

[Service]
User=webpty
Group=webpty
UMask=0077
StateDirectory=webpty
StateDirectoryMode=0700
WorkingDirectory=/var/lib/webpty
Environment=WEBPTY_ADDRESS=127.0.0.1:8000
Environment=WEBPTY_PUBLIC_ORIGIN=https://pty.example.com
Environment=WEBPTY_DATABASE_PATH=/var/lib/webpty/webpty.db
ExecStartPre=/usr/local/bin/webpty doctor
ExecStart=/usr/local/bin/webpty serve
KillSignal=SIGTERM
TimeoutStopSec=60
Restart=on-failure
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

`ExecStartPre=webpty doctor` keeps a broken configuration from starting.
webpty stops gracefully on SIGTERM: it stops accepting connections, ends
terminals (SIGTERM, then SIGKILL after `WEBPTY_TERMINAL_KILL_GRACE`), flushes
recordings, and closes the database. Keep `TimeoutStopSec` above
`WEBPTY_SHUTDOWN_TIMEOUT + WEBPTY_TERMINAL_SHUTDOWN_TIMEOUT + WEBPTY_RECORDING_SHUTDOWN_TIMEOUT`.

Hardening options such as `ProtectSystem=strict` or `PrivateTmp=true` also
apply to every terminal, which is usually not what you want from a shell;
add them only if your terminals should be confined the same way.

### launchd (macOS)

```xml
<!-- ~/Library/LaunchAgents/com.github.webpty.plist -->
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.github.webpty</string>
  <key>ProgramArguments</key>
  <array><string>/opt/homebrew/bin/webpty</string><string>serve</string></array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>WEBPTY_DATABASE_PATH</key><string>/Users/you/Library/Application Support/webpty/webpty.db</string>
  </dict>
  <key>Umask</key><integer>63</integer>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>/Users/you/Library/Logs/webpty.log</string>
</dict>
</plist>
```

Create the database directory with `mkdir -m 700` first, then
`launchctl load ~/Library/LaunchAgents/com.github.webpty.plist`.

## Docker

The image runs webpty as uid 10001 with its database on the `/data` volume,
includes `/bin/sh` (BusyBox) for terminals, and has a health check on
`/healthz`. The health check probes `127.0.0.1` on the port in
`WEBPTY_ADDRESS`, so `-e WEBPTY_ADDRESS=0.0.0.0:9000` keeps it working. If
you bind one specific non-loopback address, or change the port with
`--port` instead of the variable, pass a matching `--health-cmd`.

```sh
docker run -d --name webpty \
  -p 127.0.0.1:8000:8000 \
  -v webpty-data:/data \
  ghcr.io/0xpiranhacodes/webpty:latest
```

Inside the container webpty listens on `0.0.0.0:8000` so the published port
works, and `WEBPTY_PUBLIC_ORIGIN` defaults to `http://localhost:8000`. Publish
the port on `127.0.0.1` as above. Behind a TLS proxy, set the origin:

```sh
docker run -d --name webpty -p 127.0.0.1:8000:8000 -v webpty-data:/data \
  -e WEBPTY_PUBLIC_ORIGIN=https://pty.example.com \
  ghcr.io/0xpiranhacodes/webpty:latest
```

Terminals run inside the container, so they see the container's filesystem
and tools, not the host's. Build a derived image to add a shell or tools:

```dockerfile
FROM ghcr.io/0xpiranhacodes/webpty:latest
USER root
RUN apk add --no-cache bash git openssh-client
USER 10001:10001
ENV WEBPTY_COMMAND=/bin/bash
```

Run `docker exec webpty webpty doctor` to check a running container, and
`docker exec webpty webpty backup --output /data/backup.db` for an online
backup (see [backup-restore.md](backup-restore.md)).

## Windows

Native Windows is not supported in this release. Run webpty inside WSL 2 (it
is a Linux install there) or in Docker Desktop. Both are separate deployment
choices with their own filesystems: terminals see the WSL distribution or the
container, not Windows itself.
