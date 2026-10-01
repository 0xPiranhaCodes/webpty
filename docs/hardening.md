# Hardening settings

These settings decide what a terminal can run, what it inherits from the
server, and how long background work may take. All of them are read once at
startup. An invalid value stops the server with an error that names the
setting.

## Which commands terminals may run

| Variable | Default | Meaning |
| --- | --- | --- |
| `WEBPTY_COMMAND_ALLOW` | empty (allow everything not denied) | Executables a terminal may run |
| `WEBPTY_COMMAND_DENY` | empty | Executables a terminal may never run |

Both settings are path lists, separated like `PATH` (`:` on macOS and Linux),
for example `WEBPTY_COMMAND_ALLOW=/bin/zsh:/bin/bash`. Every entry must be an
absolute path.

A command is checked in three forms: the path as given (cleaned), the path
found through `PATH`, and that path with symlinks resolved. A command is
refused if any of its forms is denied. If the allow list is set, at least one
form must be on it. Deny wins over allow, so a symlink or alias of a denied
binary is denied as well.

The server's own default command (`WEBPTY_COMMAND`, otherwise `$SHELL`)
must pass the policy, or the server refuses to start. When an administrator
asks for a refused command, the API answers `403` with code
`command_denied`, and the audit log records `terminal.session.denied` with
the command and `reason=command_policy`.

## What terminals inherit from the server

A terminal does not inherit webpty's environment. It receives only:

- `HOME`, `USER`, `LOGNAME`, `SHELL`, `PATH`, `LANG`, `TZ`, `TMPDIR`
- every `LC_*` locale variable
- the variables named in `WEBPTY_CHILD_ENV_PASSTHROUGH`
- `TERM=xterm-256color`, always set by webpty

Anything else in the server's environment, such as cloud credentials, tokens,
or `DATABASE_URL`, stays out of terminals unless you name it.
`WEBPTY_*` variables are never passed, even if named. The server's `TERM` is
never passed either, because the browser terminal decides what it supports.

| Variable | Default | Meaning |
| --- | --- | --- |
| `WEBPTY_CHILD_ENV_PASSTHROUGH` | empty | Comma-separated variable names to pass through, for example `EDITOR,SSH_AUTH_SOCK` |

Names must match `[A-Za-z_][A-Za-z0-9_]*` and must not start with `WEBPTY_`.

## Background timing

| Variable | Default | Meaning |
| --- | --- | --- |
| `WEBPTY_RECORDING_START_TIMEOUT` | `1s` | How long a terminal waits for its recording to start. If it runs out, the terminal still opens but runs unrecorded, and owners see the recording reported as incomplete. |
| `WEBPTY_SESSION_SWEEP_INTERVAL` | `10m` | How often expired administrator and guest sessions are deleted from the database |

Durations use Go syntax, for example `500ms`, `30s` or `10m`.

## Attempt limits

Guessing is limited per client address. IPv4 addresses count individually;
IPv6 addresses count per /64 network, because one host usually controls a
whole /64.

| What | Limit |
| --- | --- |
| Administrator sign-in | 10 attempts per minute |
| Password change | 5 attempts per 5 minutes, counted both per address and per session |
| Opening an invitation link | 30 attempts per minute |

Each limit uses a fixed window that starts with the first attempt. Once a
client is over the limit, every attempt is refused with `429` until the window
ends, including one with the right password. `Retry-After` gives the seconds
left in the window, rounded up. The first refusal in each window is recorded
in the audit log (`admin.login.throttled`, `admin.password.throttled` or
`access.redeem.throttled`).

## Listening beyond this machine

If `WEBPTY_ADDRESS` can be reached from other hosts (anything other than a
loopback address), `WEBPTY_PUBLIC_ORIGIN` must be set to the URL browsers
use, for example `https://pty.example.com`. Otherwise the server refuses to
start. The origin is what cross-site and host checks compare against.
