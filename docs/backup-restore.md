# Backup, restore, and disaster recovery

All webpty state lives in one SQLite database (`WEBPTY_DATABASE_PATH`,
default `webpty.db`): the administrator password hash, sign-in and guest
sessions, share links, the audit log, terminal history, and recordings. Live
terminal processes are not state; they end when the server stops.

The database holds password and session hashes and every recorded
keystroke and output. Store backups as carefully as the database itself.

## Backing up

```sh
webpty backup --output /backups/webpty-$(date +%Y%m%dT%H%M%S).db
```

- **Safe while serving.** The copy is taken inside one SQLite read
  transaction (`VACUUM INTO`), so the server keeps running and the backup
  holds whole transactions only.
- **Never overwrites.** An existing `--output` file is refused, including
  one that appears while the backup is being written, on filesystems
  without hard links too.
- **Private.** The backup is written with mode 600, fsynced, and published
  atomically, so a partial file never appears at `--output`.
- **Verified.** Before reporting success, webpty runs an integrity check on
  the copy and confirms its schema version matches the source.
- **Self-describing.** The backup records its format, creation time, the
  schema version, and the webpty version and commit that wrote it.

`--database` (or `WEBPTY_DATABASE_PATH`) selects the source. A missing,
corrupt, or uninitialized source is an error and creates nothing.

Do not copy the database file with `cp` while webpty runs: committed data may
still be in the `-wal` file, and the copy can be torn.

Run `backup` as the user webpty runs as. Reading a stopped database makes
SQLite create its `-wal` and `-shm` files, owned by whoever ran the command,
and a server running as another user cannot use files that root left behind.

### Scheduling

```cron
# crontab for the webpty user: hourly backup, keep 7 days
0 * * * * WEBPTY_DATABASE_PATH=/var/lib/webpty/webpty.db /usr/local/bin/webpty backup --output /var/backups/webpty/webpty-$(date +\%Y\%m\%dT\%H\%M).db && find /var/backups/webpty -name 'webpty-*.db' -mtime +7 -delete
```

With Docker:

```sh
docker exec webpty webpty backup --output /data/backup.db
docker cp webpty:/data/backup.db ./webpty-backup.db
```

## Restoring

Restore is an offline operation. Stop the server first:

```sh
systemctl stop webpty
webpty restore --input /backups/webpty-20261001T120000.db
systemctl start webpty
```

Before anything is replaced, webpty:

1. takes the database lock, and refuses with "database is in use" while a
   server holds it;
2. copies the input to a private staging file next to the database;
3. checks SQLite integrity and foreign keys, and that every applied
   migration is one this build knows, unchanged;
4. refuses a backup from a **newer** webpty (a newer schema) instead of
   guessing; upgrade webpty first. A backup from an older webpty is accepted
   and migrated when the server next starts.

Then it keeps the current database as a rollback copy
(`webpty.db.pre-restore-<UTC timestamp>`), fsyncs, and atomically renames the
staged file into place. It verifies the result once more; if that fails, the
original database is put back. On every failure the original stays where it
was, and the error says where the rollback copy is if one was made.

The input can be a file written by `webpty backup` or a plain webpty
database file that has no uncheckpointed `-wal` file. Restoring the live
database onto itself is refused.

Restoring into a path with no database yet (a new machine) creates it.

### Rolling back a restore

The previous database is kept until you remove it:

```sh
systemctl stop webpty
webpty restore --input /var/lib/webpty/webpty.db.pre-restore-20261001T120501.000000000Z
systemctl start webpty
```

After a clean shutdown the rollback copy is a single file. If it has a
`-wal` companion (the server was killed before the restore), restore refuses
it; with the server stopped, move the copy and its `-wal` back to the
database path by hand instead.

## Disaster recovery checklist

1. Install the same or a newer webpty release
   ([upgrading.md](upgrading.md)); a newer schema cannot be restored by an
   older binary.
2. Recreate the service user and a private data directory (mode 700).
3. `webpty restore --input <latest backup> --database <path>`.
4. `webpty doctor` with the production environment.
5. Start the server. Administrator and guest sessions that expired while the
   server was down are swept; share links keep their original expiry.
6. Sign in and review the *Audit log* for the period before the incident.

Test this regularly: restore the latest backup into a scratch path with
`webpty restore --input backup.db --database /tmp/restore-test/webpty.db`
followed by `webpty doctor --database /tmp/restore-test/webpty.db`.
