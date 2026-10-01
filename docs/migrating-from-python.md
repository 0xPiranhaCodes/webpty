# Migrating from the original webpty

webpty releases up to 3.9 on PyPI were a Python/Tornado server
(`pip install webpty`). This version is a complete rewrite as one Go binary
with an embedded web console. The Python implementation is no longer
maintained, and its code, PyPI packaging, and publishing workflow have been
removed from this repository.

## There is nothing to import

The Python server kept no persistent data: no accounts, settings, history,
or recordings. The Go server keeps everything in its own SQLite database,
which starts empty on first run. There is no import step and no data to
carry over.

## Install the new version

Uninstall the Python package so its `webpty` script does not shadow the new
binary, then install one of the [new packages](../README.md#install):

```sh
pip uninstall webpty
curl -fsSL https://raw.githubusercontent.com/0xPiranhaCodes/webpty/main/scripts/install.sh | sh
hash -r && webpty version
```

## What changed

| Python webpty | Go webpty |
| --- | --- |
| Listened on all interfaces (`0.0.0.0:8000`) | Listens on `127.0.0.1:8000`; other addresses require `WEBPTY_PUBLIC_ORIGIN` and should sit behind TLS ([deployment.md](deployment.md)) |
| No password unless `-pass` was given; the password was kept in a cookie | An administrator password, `CHANGEME` on first run, which must be changed before anything else; stored as an Argon2id hash |
| Every browser shared one PTY | Any number of terminals, each opened by the administrator and shared through expiring, revocable editor or viewer links |
| `--cmd` defaulted to `bash` | `--cmd` defaults to `$SHELL`; it names one executable, never run through a shell, and its arguments go after `--` |
| The shell inherited the server's environment | Terminals get an allowlisted environment ([hardening.md](hardening.md)) |
| No recording or audit | Sessions are recorded by default, with playback and asciicast export; an audit log records sign-ins, shares, and terminals |
| Heroku `Procfile`, iframe embedding demo | Binary archives, Homebrew, install script, and a container image |

## Flags

| Old flag | New behavior |
| --- | --- |
| `-p`, `--port PORT` | Same. Keeps the host `127.0.0.1` unless `WEBPTY_ADDRESS` says otherwise. |
| `-c`, `--cmd CMD` | Same flag; `CMD` is one executable. Pass arguments after `--`: `webpty -c tmux -- new -A`. |
| `-ah`, `--allowed-hosts HOSTS` | Accepted with a warning and ignored. Set `--public-origin https://your.host` instead. |
| `-k`, `--keepalive SECONDS` | Accepted with a warning and ignored. See `WEBPTY_WS_PING_INTERVAL`. |
| `-pass`, `--password` | Refused, so a password never appears in the process list. Sign in with `CHANGEME` and set a password in the browser. |

## Embedding in an iframe

The Python version shipped an iframe demo. The new console sends
`X-Frame-Options: DENY` and a `frame-ancestors 'none'` content security
policy, because a framed terminal can be used for clickjacking. Link to
webpty instead of framing it.

## Windows

Like the Python version, this release does not run on native Windows. Run
it under WSL 2 or in Docker ([deployment.md](deployment.md#windows)).
