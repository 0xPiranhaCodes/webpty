# Configuration reference

webpty reads its configuration once at startup from `WEBPTY_*` environment
variables and from `webpty serve` flags. A flag overrides its variable. The
combined result is validated as a whole, and an invalid value stops the
server with an error that names the setting. `webpty doctor` accepts the same
flags and checks a configuration without starting anything.

Durations use Go syntax (`500ms`, `30s`, `10m`, `12h`). Sizes are bytes.

## Command line

```text
webpty [serve] [flags] [-- command arguments]
webpty version [--json]
webpty doctor [serve flags]
webpty backup --output PATH [--database PATH]
webpty restore --input PATH [--database PATH]
webpty help
```

`serve` is the default, so `webpty -p 9000` and `webpty serve -p 9000` are
the same.

| Flag | Variable | Default | Meaning |
| --- | --- | --- | --- |
| `--address HOST:PORT` | `WEBPTY_ADDRESS` | `127.0.0.1:8000` | Listen address. A non-loopback address requires a public origin. |
| `-p`, `--port PORT` | | | Listen port, keeping the configured host. Cannot be combined with `--address`. |
| `--database PATH` | `WEBPTY_DATABASE_PATH` | `webpty.db` | SQLite database. Its directory should be private (mode 700). |
| `--public-origin URL` | `WEBPTY_PUBLIC_ORIGIN` | empty | `scheme://host[:port]` that browsers use, for example `https://pty.example.com`. |
| `--secure-cookies` | `WEBPTY_SECURE_COOKIES` | on for an `https` origin | Mark cookies `Secure`. |
| `-c`, `--cmd EXECUTABLE` | `WEBPTY_COMMAND` | `$SHELL`, else `/bin/sh` | Program terminals run. Never interpreted by a shell. |
| `-- ARGS...` | | none | Arguments for `--cmd`, passed verbatim. Requires `--cmd`. |

`--cmd` names a single executable; `--cmd "ls -la"` looks for a program
literally called `ls -la`. To pass arguments, put them after `--`:

```sh
webpty --cmd /usr/bin/tmux -- new-session -A -s main
```

### Flags kept from the original webpty

| Flag | Behavior |
| --- | --- |
| `-p`, `--port` | Supported, as above. |
| `-c`, `--cmd` | Supported, as above. |
| `-ah`, `--allowed-hosts` | Accepted with a deprecation warning and ignored. webpty accepts only loopback hosts, or the host of `--public-origin` when set. |
| `-k`, `--keepalive` | Accepted with a deprecation warning and ignored. Idle connections are probed every `WEBPTY_WS_PING_INTERVAL`. |
| `--password`, `--pass` | Refused. The administrator password starts as `CHANGEME` and must be changed at first sign-in; it is never passed on the command line. |

## Network and cookies

| Variable | Default | Meaning |
| --- | --- | --- |
| `WEBPTY_ADDRESS` | `127.0.0.1:8000` | Listen address |
| `WEBPTY_PUBLIC_ORIGIN` | empty | Public URL; required off loopback. Cross-site, origin, and host checks compare against it. |
| `WEBPTY_SECURE_COOKIES` | `true` if the origin is `https` | `Secure` cookie attribute |
| `WEBPTY_READ_HEADER_TIMEOUT` | `5s` | Time to read request headers |
| `WEBPTY_READ_TIMEOUT` | `15s` | Time to read a request |
| `WEBPTY_WRITE_TIMEOUT` | `30s` | Time to write an HTTP response |
| `WEBPTY_IDLE_TIMEOUT` | `120s` | Keep-alive idle time |
| `WEBPTY_SHUTDOWN_TIMEOUT` | `10s` | How long shutdown waits for HTTP requests |
| `WEBPTY_WS_WRITE_TIMEOUT` | `10s` | Bound on each WebSocket message write |
| `WEBPTY_WS_PING_INTERVAL` | `30s` | How often idle WebSocket peers are probed |

## Administrator and guest sessions

| Variable | Default | Meaning |
| --- | --- | --- |
| `WEBPTY_SESSION_TTL` | `12h` | Administrator sign-in lifetime |
| `WEBPTY_BOOTSTRAP_TTL` | `10m` | Lifetime of a first-run `CHANGEME` sign-in, which can only change the password |
| `WEBPTY_SESSION_SWEEP_INTERVAL` | `10m` | How often expired sessions are deleted |
| `WEBPTY_ACCESS_SESSION_TTL` | `12h` | Longest a guest session lasts; never longer than its link |
| `WEBPTY_GRANT_DEFAULT_TTL` | `24h` | Lifetime of a share link created without one (at least `1m`) |
| `WEBPTY_GRANT_MAX_TTL` | `168h` | Longest lifetime an administrator may give a link |
| `WEBPTY_GRANT_MAX_REDEMPTIONS` | `100` | Default and maximum uses of one link |

## Terminals

| Variable | Default | Meaning |
| --- | --- | --- |
| `WEBPTY_COMMAND` | `$SHELL`, else `/bin/sh` | Default program |
| `WEBPTY_COMMAND_ALLOW` | empty | Executables terminals may run (`:`-separated absolute paths) |
| `WEBPTY_COMMAND_DENY` | empty | Executables terminals may never run |
| `WEBPTY_CHILD_ENV_PASSTHROUGH` | empty | Comma-separated extra variables passed to terminals |
| `WEBPTY_MAX_SESSIONS` | `16` | Concurrent terminals |
| `WEBPTY_MAX_VIEWERS` | `8` | Connections per terminal |
| `WEBPTY_TERMINAL_IDLE_TIMEOUT` | `1h` | An idle terminal is ended after this long |
| `WEBPTY_TERMINAL_KILL_GRACE` | `5s` | Time an ended terminal's process group has between SIGTERM and SIGKILL |
| `WEBPTY_TERMINAL_SHUTDOWN_TIMEOUT` | kill grace + `5s` | How long server shutdown waits for terminals; must exceed the kill grace |
| `WEBPTY_REPLAY_BYTES` | `262144` | Recent output replayed to a newly connected browser |
| `WEBPTY_CLIENT_QUEUE_BYTES` | `1048576` | Output buffered per slow browser before it is disconnected; at least the replay size |

The command policy and the inherited environment are described in
[hardening.md](hardening.md).

## Recording

| Variable | Default | Meaning |
| --- | --- | --- |
| `WEBPTY_RECORDING_ENABLED` | `true` | Record new terminals unless the request opts out |
| `WEBPTY_RECORDING_RETENTION` | `720h` | Ended recordings older than this are deleted |
| `WEBPTY_RECORDING_MAX_BYTES` | `268435456` | Per-recording limit; a recording over it is kept and marked incomplete |
| `WEBPTY_RECORDING_FLUSH_INTERVAL` | `1s` | How often buffered events are written |
| `WEBPTY_RECORDING_QUEUE_BYTES` | `1048576` | In-memory buffer per recording (at least 16 KiB) |
| `WEBPTY_RECORDING_CHUNK_BYTES` | `65536` | Stored chunk size (1 KiB to 1 MiB) |
| `WEBPTY_RECORDING_CHUNK_EVENTS` | `1024` | Events per stored chunk (at most 65536) |
| `WEBPTY_RECORDING_START_TIMEOUT` | `1s` | How long a terminal waits for its recording to start |
| `WEBPTY_RECORDING_SHUTDOWN_TIMEOUT` | `5s` | How long shutdown waits to flush recordings |

## Files webpty writes

| Path | Contents |
| --- | --- |
| `WEBPTY_DATABASE_PATH` | Everything: password hash, sessions, share links, audit log, recordings |
| `…-wal`, `…-shm` | SQLite write-ahead log and shared memory, next to the database |
| `….lock` | Held while the server runs, so `restore` cannot replace a live database. Created by `serve` and `restore`, never by `doctor` or `backup`, and kept after exit (the lock is on the file, not its existence). It must be a regular file the service user can write |
| `….pre-restore-*` | Rollback copies left by `webpty restore` |

The lock file, backups, and restored databases are created with mode 600.
SQLite creates a new database and its `-wal`/`-shm` files with the process
umask, so start webpty with `umask 077` or keep the files in a directory
only the webpty user can enter (mode 700). `webpty doctor` warns when the
database can be read by other users.
