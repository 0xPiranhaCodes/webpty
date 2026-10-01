# Troubleshooting and `webpty doctor`

## Start with doctor

`webpty doctor` checks this host and a configuration without starting the
server. It does not create or change the database, its lock file, or its
write-ahead log; the only file it writes is a probe in the database
directory, which it removes straight away. This matters when you run it as
root against a service's database. It takes the same flags and `WEBPTY_*`
variables as `webpty serve`, so run it with exactly the environment the
server uses:

```console
$ webpty doctor
webpty 1.3.0 (commit 3e9ca7b…, built 2026-10-01T06:00:00Z, go1.26.8, darwin/arm64)

[ OK ] configuration: environment and flags are valid
[ OK ] platform: darwin/arm64 is supported
[ OK ] database directory: . is writable and not writable by other users
[ OK ] database: webpty.db does not exist yet; it will be created on first start
[ OK ] database lock: no lock file; webpty is not running and will create webpty.db.lock
[ OK ] migrations: 6 embedded migrations, ordered and checksummed
[ OK ] command: /bin/zsh (/bin/zsh) with no arguments; no allow or deny list; 0 extra environment variables passed to terminals
[WARN] network: 127.0.0.1:8000 is already in use (is webpty already running?)
[ OK ] frontend assets: embedded index.html and its 4 bundled assets
0 failed, 1 warnings, 8 checks
```

It exits 0 when nothing failed (warnings allowed) and 1 otherwise, so it can
gate a service start (`ExecStartPre=webpty doctor`). Its output names
settings, paths, and counts, and never prints passwords, tokens, command
arguments, or environment values, so it is safe to paste into an issue.

| Check | What it verifies |
| --- | --- |
| configuration | Every `WEBPTY_*` variable and flag parses and the combination is valid |
| platform | macOS or Linux on amd64 or arm64 |
| database directory | Exists, is writable, and other users cannot replace files in it |
| database | Integrity, schema version, pending migrations, and file permissions |
| database lock | Whether a server holds `<database>.lock`, and that this user could take it (without creating it) |
| migrations | The migrations built into this binary are ordered and checksummed |
| command | The terminal command exists, is executable, and passes the allow/deny policy |
| network | The address can be bound; loopback, public origin, and secure-cookie settings are compatible |
| frontend assets | The web UI is embedded and every asset it references is present |

## Common problems

**`WEBPTY_ADDRESS "0.0.0.0:8000" is reachable from other hosts; set WEBPTY_PUBLIC_ORIGIN`**
webpty will not listen beyond loopback without knowing its public URL. Set
`WEBPTY_PUBLIC_ORIGIN=https://your.host` and put a TLS proxy in front
([deployment.md](deployment.md)), or keep the default loopback address.

**`untrusted host; configure WEBPTY_PUBLIC_ORIGIN` or `cross-origin request rejected`**
The browser's URL does not match the server's idea of itself. Without a
public origin, only `localhost`, `127.0.0.1`, and `[::1]` are accepted. With
one, it must match the browser's address bar exactly, including scheme and
port, and the proxy must pass the `Origin` header.

**Signed in, then immediately signed out (behind a proxy)**
Secure cookies are on but the browser reached webpty over plain HTTP, or the
origin is `https` but the proxy serves HTTP. Use HTTPS end to end at the
browser, or set `WEBPTY_SECURE_COOKIES=false` only for a loopback test.

**The terminal connects and immediately closes (behind a proxy)**
The proxy is not passing WebSocket upgrades, or it times out idle
connections. See the nginx example in [deployment.md](deployment.md).

**`database is in use by a running webpty; stop it first`**
Another server holds `webpty.db.lock`. Two servers cannot share a database,
and `webpty restore` needs the server stopped. Stop the other process; the
lock is released automatically when a process exits, even if it crashed.

**`database lock: … permission denied` or `… is owned by uid N and not writable by uid M`**
The lock file belongs to another user. Usually the service runs as a
dedicated account and doctor or a maintenance command ran as you, or root
started webpty once. `webpty serve` as the service user cannot take a lock
it cannot open for writing. Run doctor, backup, and restore as the service
user (`sudo -u webpty webpty doctor …`). If root created the file, give it
back: `chown webpty: /var/lib/webpty/webpty.db.lock`. Never delete the lock
while a server may be running; the file is what the server locks.
`… is a symbolic link` or `… is not a regular file` means something replaced
the lock path; remove it while webpty is stopped.

**`database schema is newer than this webpty supports`**
The database was used by a newer webpty. Install that version or newer
([upgrading.md](upgrading.md)). Downgrades need a backup from before the
upgrade.

**`migration … changed after it was applied` or `database integrity check failed`**
The database was modified outside webpty or is damaged. Stop the server,
keep the damaged file for analysis, and restore the latest backup
([backup-restore.md](backup-restore.md)).

**`… is not permitted by WEBPTY_COMMAND_ALLOW/WEBPTY_COMMAND_DENY`**
The default command fails the command policy, so the server refuses to
start. Allow it or choose another `WEBPTY_COMMAND`. The policy checks the
path as given, as found on `PATH`, and with symlinks resolved
([hardening.md](hardening.md)).

**Programs in a terminal are missing environment variables**
Terminals receive only an allowlisted environment. Name what they need in
`WEBPTY_CHILD_ENV_PASSTHROUGH`, for example `EDITOR,SSH_AUTH_SOCK`.

**`the web UI is not embedded in this build`**
The binary was built with `go build` before the frontend. Use a release
archive, or `make build` from a source checkout.

**Forgot the administrator password**
There is no recovery flow that bypasses the password. Stop the server,
back up the database, then start with a fresh `WEBPTY_DATABASE_PATH`; the
new database starts with `CHANGEME` again. Recordings and audit history stay
in the old file.

**Too many sign-in attempts (`429`)**
Sign-in, password change, and invitation redemption are rate limited per
client address ([hardening.md](hardening.md#attempt-limits)). Behind a proxy
every client shares the proxy's address. Wait for `Retry-After` seconds.

## Logs

webpty logs key=value lines to standard error: terminal starts and exits,
denied commands, disconnected viewers, recording problems, and errors. Under systemd use `journalctl -u webpty`; in Docker
use `docker logs webpty`. The audit log in the console (*Audit log*) is the
record of who signed in, shared, opened, and ended what.
