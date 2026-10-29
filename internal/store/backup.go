package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// BackupFormat identifies the backup layout written by Backup.
const BackupFormat = "webpty-backup-v1"

// backupTable holds a backup's metadata inside the backup file. Restore
// drops it before the database goes live.
const backupTable = "webpty_backup"

// ErrNoBackupMetadata reports a database file that Backup did not write.
var ErrNoBackupMetadata = errors.New("file has no webpty backup metadata")

// BackupOptions names the webpty build writing a backup.
type BackupOptions struct {
	Version string
	Commit  string
}

// BackupMetadata describes a backup file.
type BackupMetadata struct {
	Format        string
	CreatedAt     time.Time
	WebptyVersion string
	WebptyCommit  string
	SchemaVersion int
}

// Backup writes a consistent snapshot of the database at src to dst. The
// snapshot is taken inside one SQLite read transaction (VACUUM INTO), so a
// running server may keep writing. Backup refuses an existing dst, writes
// the file with mode 0600, records metadata in it, and checks its integrity
// and schema before it appears at dst.
func Backup(ctx context.Context, src, dst string, opts BackupOptions) (meta BackupMetadata, err error) {
	if err := checkBackupPaths(src, dst); err != nil {
		return BackupMetadata{}, err
	}
	source, err := openFile(src, "ro")
	if err != nil {
		return BackupMetadata{}, fmt.Errorf("backup: %w", err)
	}
	defer source.Close()
	version, err := sourceSchemaVersion(ctx, source, src)
	if err != nil {
		return BackupMetadata{}, err
	}
	tmp, err := createHiddenTemp(dst, ".partial-*")
	if err != nil {
		return BackupMetadata{}, fmt.Errorf("backup: %w", err)
	}
	defer removeDatabaseFiles(tmp)
	if meta, err = writeSnapshot(ctx, source, tmp, opts, version); err != nil {
		return BackupMetadata{}, err
	}
	if err := publishBackup(tmp, dst); err != nil {
		return BackupMetadata{}, err
	}
	return meta, nil
}

// checkBackupPaths refuses an existing dst and a src that is not a regular file.
func checkBackupPaths(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("backup: %s: %w", dst, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("backup: %w", err)
	}
	if info, err := os.Stat(src); err != nil {
		return fmt.Errorf("backup: %w", err)
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("backup: %s is not a regular file", src)
	}
	return nil
}

func sourceSchemaVersion(ctx context.Context, source *sql.DB, src string) (int, error) {
	inspection, err := inspectDB(ctx, source)
	if err != nil {
		return 0, fmt.Errorf("backup: %s: %w", src, err)
	}
	if inspection.SchemaVersion == 0 {
		return 0, fmt.Errorf("backup: %s is not an initialized webpty database", src)
	}
	return inspection.SchemaVersion, nil
}

// createHiddenTemp creates an empty private file next to path and returns
// its name; pattern is appended to the hidden base name.
func createHiddenTemp(path, pattern string) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	_ = f.Close()
	return name, nil
}

// writeSnapshot copies source into tmp, stamps and verifies it, and fsyncs it.
func writeSnapshot(ctx context.Context, source *sql.DB, tmp string, opts BackupOptions, version int) (BackupMetadata, error) {
	// VACUUM INTO accepts an existing empty file, which keeps CreateTemp's
	// private mode from the first byte written.
	if _, err := source.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return BackupMetadata{}, fmt.Errorf("backup: snapshot: %w", err)
	}
	meta := BackupMetadata{
		Format:        BackupFormat,
		CreatedAt:     time.Now().UTC().Truncate(time.Second),
		WebptyVersion: opts.Version,
		WebptyCommit:  opts.Commit,
		SchemaVersion: version,
	}
	if err := finishBackup(ctx, tmp, meta); err != nil {
		return BackupMetadata{}, fmt.Errorf("backup: %w", err)
	}
	if err := syncFile(tmp); err != nil {
		return BackupMetadata{}, fmt.Errorf("backup: %w", err)
	}
	return meta, nil
}

// publishBackup puts tmp at dst. Both a hard link and an O_EXCL copy (for
// filesystems without hard links) publish the file without replacing
// anything that appeared at dst in the meantime.
func publishBackup(tmp, dst string) error {
	if err := linkFile(tmp, dst); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("backup: %s: %w", dst, os.ErrExist)
		}
		if err := copyExclusive(tmp, dst); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}

var linkFile = os.Link

func copyExclusive(src, dst string) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := copyInto(out, src); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

func finishBackup(ctx context.Context, path string, meta BackupMetadata) error {
	db, err := openFile(path, "rw")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE `+backupTable+` (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT`); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}
	for key, value := range map[string]string{
		"format":         meta.Format,
		"created_at":     meta.CreatedAt.Format(time.RFC3339),
		"webpty_version": meta.WebptyVersion,
		"webpty_commit":  meta.WebptyCommit,
		"schema_version": strconv.Itoa(meta.SchemaVersion),
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO `+backupTable+` (key, value) VALUES (?, ?)`, key, value); err != nil {
			return fmt.Errorf("write metadata: %w", err)
		}
	}
	inspection, err := inspectDB(ctx, db)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	if inspection.SchemaVersion != meta.SchemaVersion {
		return fmt.Errorf("verify: backup has schema version %d, source had %d", inspection.SchemaVersion, meta.SchemaVersion)
	}
	return nil
}

// ReadBackupMetadata returns the metadata Backup stored in path.
func ReadBackupMetadata(ctx context.Context, path string) (BackupMetadata, error) {
	if _, err := os.Stat(path); err != nil {
		return BackupMetadata{}, err
	}
	db, err := openFile(path, "ro")
	if err != nil {
		return BackupMetadata{}, err
	}
	defer db.Close()
	return readBackupMetadata(ctx, db)
}

func readBackupMetadata(ctx context.Context, db *sql.DB) (BackupMetadata, error) {
	if err := requireBackupTable(ctx, db); err != nil {
		return BackupMetadata{}, err
	}
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM `+backupTable)
	if err != nil {
		return BackupMetadata{}, fmt.Errorf("read backup metadata: %w", err)
	}
	defer rows.Close()
	var meta BackupMetadata
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return BackupMetadata{}, fmt.Errorf("read backup metadata: %w", err)
		}
		if err := meta.set(key, value); err != nil {
			return BackupMetadata{}, fmt.Errorf("read backup metadata: %s: %w", key, err)
		}
	}
	if err := rows.Err(); err != nil {
		return BackupMetadata{}, fmt.Errorf("read backup metadata: %w", err)
	}
	if meta.Format != BackupFormat {
		return BackupMetadata{}, fmt.Errorf("read backup metadata: unsupported format %q", meta.Format)
	}
	return meta, nil
}

func requireBackupTable(ctx context.Context, db *sql.DB) error {
	var present int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, backupTable).Scan(&present); err != nil {
		return fmt.Errorf("read backup metadata: %w", err)
	}
	if present == 0 {
		return ErrNoBackupMetadata
	}
	return nil
}

// set stores one metadata row; unknown keys are ignored for forward compatibility.
func (m *BackupMetadata) set(key, value string) (err error) {
	switch key {
	case "format":
		m.Format = value
	case "created_at":
		m.CreatedAt, err = time.Parse(time.RFC3339, value)
	case "webpty_version":
		m.WebptyVersion = value
	case "webpty_commit":
		m.WebptyCommit = value
	case "schema_version":
		m.SchemaVersion, err = strconv.Atoi(value)
	}
	return err
}

// RestoreResult describes a completed restore.
type RestoreResult struct {
	// Metadata is nil when the input was a plain database file rather than
	// a file written by Backup.
	Metadata      *BackupMetadata
	SchemaVersion int
	// RollbackPath holds the replaced database; empty if there was none.
	RollbackPath string
}

// afterRestoreSwap, when set by tests, runs after the restored database is
// in place and before its final verification.
var afterRestoreSwap func(target string) error

// Restore replaces the database at target with input. It must run while
// no server uses target: it takes the database lock and fails with
// ErrDatabaseInUse otherwise. The input is copied to a private staging file
// next to target and must pass an integrity check and carry a schema this
// build supports (ErrNewerSchema otherwise) before anything is replaced.
// The replaced database is kept at RollbackPath, the swap is an fsynced
// atomic rename, and any failure leaves the original database at target.
func Restore(ctx context.Context, input, target string) (result RestoreResult, err error) {
	release, err := LockDatabase(target)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("restore: %w", err)
	}
	defer func() { _ = release() }()
	targetExists, err := checkRestoreInput(input, target)
	if err != nil {
		return RestoreResult{}, err
	}
	stagingPath, metadata, version, err := stageRestore(ctx, input, target)
	if err != nil {
		return RestoreResult{}, err
	}
	swapped := false
	defer func() {
		if !swapped {
			removeDatabaseFiles(stagingPath)
		}
	}()
	rollback, err := keepRollback(target, targetExists)
	if err != nil {
		return RestoreResult{}, err
	}
	if swapped, err = swapIn(ctx, stagingPath, target, rollback, version); err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{Metadata: metadata, SchemaVersion: version, RollbackPath: rollback}, nil
}

// checkRestoreInput rejects an input that is not a regular file, is target
// itself, or still has committed pages in its write-ahead log.
func checkRestoreInput(input, target string) (targetExists bool, err error) {
	inputInfo, err := os.Stat(input)
	if err != nil {
		return false, fmt.Errorf("restore: %w", err)
	}
	if !inputInfo.Mode().IsRegular() {
		return false, fmt.Errorf("restore: %s is not a regular file", input)
	}
	targetInfo, err := os.Stat(target)
	targetExists = err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("restore: %w", err)
	}
	if targetExists && os.SameFile(inputInfo, targetInfo) {
		return false, errors.New("restore: the input is the database being replaced")
	}
	if nonEmpty(input + "-wal") {
		return false, fmt.Errorf("restore: %s has an uncheckpointed write-ahead log; "+
			"stop the server that uses it, or make a copy with webpty backup", input)
	}
	return targetExists, nil
}

// stageRestore copies input to a private file next to target and checks
// it. On error the staging file is already removed.
func stageRestore(ctx context.Context, input, target string) (path string, metadata *BackupMetadata, version int, err error) {
	staging, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".restore-*")
	if err != nil {
		return "", nil, 0, fmt.Errorf("restore: %w", err)
	}
	stagingPath := staging.Name()
	defer func() {
		if err != nil {
			removeDatabaseFiles(stagingPath)
		}
	}()
	if err := copyInto(staging, input); err != nil {
		return "", nil, 0, fmt.Errorf("restore: %w", err)
	}
	if metadata, version, err = prepareRestore(ctx, stagingPath); err != nil {
		return "", nil, 0, fmt.Errorf("restore: %s: %w", input, err)
	}
	if err := syncFile(stagingPath); err != nil {
		return "", nil, 0, fmt.Errorf("restore: %w", err)
	}
	return stagingPath, metadata, version, nil
}

// rollbackTimeFormat names rollback copies so they sort by restore time.
const rollbackTimeFormat = "20060102T150405.000000000Z"

// keepRollback links the current database, and its write-ahead log, to a
// timestamped rollback path. It returns "" when there is no database yet.
func keepRollback(target string, targetExists bool) (string, error) {
	if !targetExists {
		return "", nil
	}
	rollback := target + ".pre-restore-" + time.Now().UTC().Format(rollbackTimeFormat)
	if err := preserve(target, rollback); err != nil {
		return "", fmt.Errorf("restore: keep rollback copy: %w", err)
	}
	if nonEmpty(target + "-wal") {
		if err := preserve(target+"-wal", rollback+"-wal"); err != nil {
			removeDatabaseFiles(rollback)
			return "", fmt.Errorf("restore: keep rollback copy: %w", err)
		}
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		removeDatabaseFiles(rollback)
		return "", fmt.Errorf("restore: %w", err)
	}
	return rollback, nil
}

// swapIn renames the staged database over target and verifies it, putting
// the original back on any failure. swapped reports whether the staging
// file was consumed by the rename.
func swapIn(ctx context.Context, stagingPath, target, rollback string, version int) (swapped bool, err error) {
	// The old write-ahead log and shared memory belong to the replaced
	// database; SQLite would replay them into the restored one.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(target + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, putBack(rollback, target, err)
		}
	}
	if err := os.Rename(stagingPath, target); err != nil {
		return false, putBack(rollback, target, err)
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		return true, putBack(rollback, target, err)
	}
	if err := verifyRestored(ctx, target, version); err != nil {
		return true, putBack(rollback, target, err)
	}
	return true, nil
}

func verifyRestored(ctx context.Context, target string, version int) error {
	if afterRestoreSwap != nil {
		if err := afterRestoreSwap(target); err != nil {
			return err
		}
	}
	inspection, err := Inspect(ctx, target)
	if err == nil && inspection.SchemaVersion != version {
		err = fmt.Errorf("restored database has schema version %d, want %d", inspection.SchemaVersion, version)
	}
	return err
}

// putBack restores the rollback copy after a failed swap and explains
// where the original database is.
func putBack(rollback, target string, cause error) error {
	if rollback == "" {
		removeDatabaseFiles(target)
		return fmt.Errorf("restore: %w", cause)
	}
	if err := reinstate(rollback, target); err != nil {
		return fmt.Errorf("restore: %w; putting the original back also failed (%v): it is kept at %s", cause, err, rollback)
	}
	return fmt.Errorf("restore: %w; the original database was put back and is also kept at %s", cause, rollback)
}

// prepareRestore checks a staged copy and removes its backup metadata.
func prepareRestore(ctx context.Context, path string) (*BackupMetadata, int, error) {
	db, err := openFile(path, "rw")
	if err != nil {
		return nil, 0, err
	}
	defer db.Close()
	inspection, err := inspectDB(ctx, db)
	if err != nil {
		return nil, 0, err
	}
	if inspection.SchemaVersion == 0 {
		return nil, 0, errors.New("not a webpty database: it has no schema")
	}
	if err := checkAllForeignKeys(ctx, db); err != nil {
		return nil, 0, err
	}
	var metadata *BackupMetadata
	meta, err := readBackupMetadata(ctx, db)
	switch {
	case err == nil:
		metadata = &meta
	case !errors.Is(err, ErrNoBackupMetadata):
		return nil, 0, err
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS `+backupTable); err != nil {
		return nil, 0, fmt.Errorf("remove backup metadata: %w", err)
	}
	// A staged file in WAL mode would keep committed pages in a -wal file
	// that the rename does not move.
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = DELETE`); err != nil {
		return nil, 0, fmt.Errorf("prepare database: %w", err)
	}
	return metadata, inspection.SchemaVersion, nil
}

func checkAllForeignKeys(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("foreign key check failed")
	}
	return rows.Err()
}

// preserve makes dst a second name for src, or a private copy where hard
// links are unsupported.
func preserve(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	} else if errors.Is(err, os.ErrExist) {
		return err
	}
	return copyExclusive(src, dst)
}

// reinstate atomically puts the rollback copy, and its write-ahead log if
// it has one, back at target.
func reinstate(rollback, target string) error {
	dir := filepath.Dir(target)
	for _, suffix := range []string{"-wal", ""} {
		if suffix == "-wal" && !nonEmpty(rollback+suffix) {
			if err := os.Remove(target + "-wal"); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		tmp := filepath.Join(dir, "."+filepath.Base(target)+".rollback-"+strconv.FormatInt(time.Now().UnixNano(), 36)+suffix)
		if err := preserve(rollback+suffix, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, target+suffix); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	_ = os.Remove(target + "-shm")
	return syncDir(dir)
}

// copyInto copies src into out, fsyncs, and closes out.
func copyInto(out *os.File, src string) error {
	in, err := os.Open(src)
	if err != nil {
		_ = out.Close()
		return err
	}
	defer in.Close()
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func nonEmpty(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

func removeDatabaseFiles(path string) {
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// syncDir makes a rename or new link in dir durable. File systems that
// cannot fsync a directory report EINVAL or ENOTSUP, which is ignored.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
