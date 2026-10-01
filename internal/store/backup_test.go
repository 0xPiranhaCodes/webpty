package store_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/store"
)

var ctx = context.Background()

func seededDB(t *testing.T, events ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "webpty.db")
	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, event := range events {
		appendEvent(t, s, event)
	}
	return path
}

func appendEvent(t *testing.T, s *store.Store, event string) {
	t.Helper()
	if err := s.AppendAuditEvent(ctx, store.AuditEvent{OccurredAt: time.Now(), Type: event}); err != nil {
		t.Fatal(err)
	}
}

func eventTypes(t *testing.T, path string) []string {
	t.Helper()
	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events, err := s.AuditEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

func backupOf(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "backup.db")
	if _, err := store.Backup(ctx, src, dst, store.BackupOptions{Version: "1.2.3", Commit: "abc1234"}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return dst
}

func TestBackupWritesAVerifiedPrivateSnapshotWithMetadata(t *testing.T) {
	src := seededDB(t, "first", "second")
	dst := filepath.Join(t.TempDir(), "backup.db")

	meta, err := store.Backup(ctx, src, dst, store.BackupOptions{Version: "1.2.3", Commit: "abc1234"})
	if err != nil {
		t.Fatal(err)
	}
	if meta.SchemaVersion != store.LatestSchemaVersion() || meta.WebptyVersion != "1.2.3" || meta.WebptyCommit != "abc1234" ||
		meta.Format != store.BackupFormat || time.Since(meta.CreatedAt) > time.Minute {
		t.Fatalf("metadata = %+v", meta)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("backup mode = %o, want 600", perm)
	}
	stored, err := store.ReadBackupMetadata(ctx, dst)
	if err != nil || stored != meta {
		t.Fatalf("stored metadata = %+v, %v; want %+v", stored, err, meta)
	}
	if got := eventTypes(t, dst); strings.Join(got, ",") != "first,second" {
		t.Errorf("backup events = %q", got)
	}
	if names := entries(t, filepath.Dir(dst)); len(names) != 1 {
		t.Errorf("backup directory = %q, want only the backup", names)
	}
}

func TestBackupRefusesToOverwrite(t *testing.T) {
	src := seededDB(t, "event")
	dst := filepath.Join(t.TempDir(), "backup.db")
	if err := os.WriteFile(dst, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Backup(ctx, src, dst, store.BackupOptions{}); err == nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want os.ErrExist", err)
	}
	if string(readFile(t, dst)) != "keep me" {
		t.Error("existing file was modified")
	}
	if names := entries(t, filepath.Dir(dst)); len(names) != 1 {
		t.Errorf("directory = %q, want no partial files", names)
	}
}

func TestBackupOfAMissingDatabaseFailsWithoutCreatingIt(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "missing.db")
	if _, err := store.Backup(ctx, src, filepath.Join(dir, "backup.db"), store.BackupOptions{}); err == nil {
		t.Fatal("backup of a missing database succeeded")
	}
	if names := entries(t, dir); len(names) != 0 {
		t.Errorf("directory = %q, want it untouched", names)
	}
}

func TestBackupOfACorruptDatabaseFailsWithoutOutput(t *testing.T) {
	src := seededDB(t, "event")
	corrupt(t, src)
	out := t.TempDir()
	if _, err := store.Backup(ctx, src, filepath.Join(out, "backup.db"), store.BackupOptions{}); err == nil {
		t.Fatal("backup of a corrupt database succeeded")
	}
	if names := entries(t, out); len(names) != 0 {
		t.Errorf("output directory = %q, want no files", names)
	}
}

// Writers commit audit events in pairs. A consistent snapshot never splits
// a transaction, so every backup holds an even number of events.
func TestBackupDuringWritesCapturesWholeTransactions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webpty.db")
	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var committed atomic.Int64
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				err := s.WithTx(ctx, func(tx *store.Tx) error {
					for i := 0; i < 2; i++ {
						if err := tx.AppendAuditEvent(ctx, store.AuditEvent{OccurredAt: time.Now(), Type: "pair"}); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					t.Error(err)
					return
				}
				committed.Add(1)
			}
		}()
	}
	for committed.Load() < 5 {
		time.Sleep(time.Millisecond)
	}
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		dst := filepath.Join(dir, "backup-"+string(rune('a'+i))+".db")
		if _, err := store.Backup(ctx, path, dst, store.BackupOptions{}); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("backup %d during writes: %v", i, err)
		}
		if n := len(eventTypes(t, dst)); n == 0 || n%2 != 0 {
			t.Errorf("backup %d holds %d events, want a positive even count", i, n)
		}
	}
	close(stop)
	wg.Wait()
}

func TestRestoreReplacesTheDatabaseAndKeepsARollbackCopy(t *testing.T) {
	target := seededDB(t, "kept")
	backup := backupOf(t, target)
	s, err := store.Open(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	appendEvent(t, s, "after-backup")
	s.Close()

	result, err := store.Restore(ctx, backup, target)
	if err != nil {
		t.Fatal(err)
	}
	if got := eventTypes(t, target); strings.Join(got, ",") != "kept" {
		t.Errorf("restored events = %q, want the backup's", got)
	}
	if result.RollbackPath == "" || !strings.HasPrefix(filepath.Base(result.RollbackPath), "webpty.db.pre-restore-") {
		t.Fatalf("rollback path = %q", result.RollbackPath)
	}
	if got := eventTypes(t, result.RollbackPath); strings.Join(got, ",") != "kept,after-backup" {
		t.Errorf("rollback copy events = %q, want the replaced database", got)
	}
	if result.Metadata == nil || result.Metadata.WebptyVersion != "1.2.3" || result.SchemaVersion != store.LatestSchemaVersion() {
		t.Errorf("result = %+v", result)
	}
	if _, err := store.ReadBackupMetadata(ctx, target); !errors.Is(err, store.ErrNoBackupMetadata) {
		t.Errorf("restored database still carries backup metadata: %v", err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("restored mode = %v, %v; want 600", info.Mode(), err)
	}
	for _, name := range entries(t, filepath.Dir(target)) {
		if strings.Contains(name, ".restore-") {
			t.Errorf("staging file %s left behind", name)
		}
	}
}

func TestRestoreIntoAMissingDatabaseCreatesIt(t *testing.T) {
	backup := backupOf(t, seededDB(t, "event"))
	target := filepath.Join(t.TempDir(), "new.db")
	result, err := store.Restore(ctx, backup, target)
	if err != nil {
		t.Fatal(err)
	}
	if result.RollbackPath != "" {
		t.Errorf("rollback path = %q, want none for a new database", result.RollbackPath)
	}
	if got := eventTypes(t, target); strings.Join(got, ",") != "event" {
		t.Errorf("events = %q", got)
	}
}

func corrupt(t *testing.T, path string) {
	t.Helper()
	data := readFile(t, path)
	// Keep the header so SQLite opens the file, then damage the b-tree pages.
	for i := 4096; i < len(data); i++ {
		data[i] = 0xA5
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertUnchanged(t *testing.T, path string, want []byte) {
	t.Helper()
	if !bytes.Equal(readFile(t, path), want) {
		t.Error("the original database was modified")
	}
	for _, name := range entries(t, filepath.Dir(path)) {
		if strings.Contains(name, ".restore-") || strings.Contains(name, ".pre-restore-") {
			t.Errorf("%s left behind by a refused restore", name)
		}
	}
}

func TestRestoreRefusesCorruptInputAndKeepsTheOriginal(t *testing.T) {
	target := seededDB(t, "original")
	original := readFile(t, target)
	for name, damage := range map[string]func(string){
		"damaged pages": func(p string) { corrupt(t, p) },
		"not sqlite":    func(p string) { _ = os.WriteFile(p, []byte("definitely not a database"), 0o600) },
		"truncated":     func(p string) { data := readFile(t, p); _ = os.WriteFile(p, data[:len(data)/3], 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			backup := backupOf(t, seededDB(t, "backup"))
			damage(backup)
			if _, err := store.Restore(ctx, backup, target); err == nil {
				t.Fatal("restore of a damaged backup succeeded")
			}
			assertUnchanged(t, target, original)
		})
	}
}

func TestRestoreRefusesANewerSchema(t *testing.T) {
	target := seededDB(t, "original")
	original := readFile(t, target)
	backup := backupOf(t, seededDB(t, "backup"))
	db, err := store.OpenRaw(backup)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name, applied_at, checksum) VALUES (?, 'future.sql', 0, 'x')`,
		store.LatestSchemaVersion()+1); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = store.Restore(ctx, backup, target)
	if !errors.Is(err, store.ErrNewerSchema) {
		t.Fatalf("err = %v, want ErrNewerSchema", err)
	}
	assertUnchanged(t, target, original)
}

func TestRestoreRefusesWhileTheDatabaseIsInUse(t *testing.T) {
	target := seededDB(t, "original")
	original := readFile(t, target)
	backup := backupOf(t, seededDB(t, "backup"))
	release, err := store.LockDatabase(target)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := store.Restore(ctx, backup, target); !errors.Is(err, store.ErrDatabaseInUse) {
		t.Fatalf("err = %v, want ErrDatabaseInUse", err)
	}
	assertUnchanged(t, target, original)
}

func TestRestoreRefusesTheDatabaseItself(t *testing.T) {
	target := seededDB(t, "original")
	if _, err := store.Restore(ctx, target, target); err == nil {
		t.Fatal("restoring a database onto itself succeeded")
	}
}

// Copying a live database's main file without its write-ahead log would
// lose committed transactions.
func TestRestoreRefusesALiveDatabaseWithAWriteAheadLog(t *testing.T) {
	live := filepath.Join(t.TempDir(), "live.db")
	s, err := store.Open(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	appendEvent(t, s, "only-in-wal")
	target := seededDB(t, "original")
	original := readFile(t, target)

	if _, err := store.Restore(ctx, live, target); err == nil || !strings.Contains(err.Error(), "webpty backup") {
		t.Fatalf("err = %v, want advice to use webpty backup", err)
	}
	assertUnchanged(t, target, original)
}

func TestInspectReportsSchemaStateWithoutChangingTheDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	if err := store.MigrateTo(ctx, path, 3); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.Inspect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.Exists || inspection.SchemaVersion != 3 || inspection.Pending != store.LatestSchemaVersion()-3 {
		t.Fatalf("inspection = %+v", inspection)
	}
	again, err := store.Inspect(ctx, path)
	if err != nil || again.SchemaVersion != 3 {
		t.Fatalf("Inspect migrated the database: %+v, %v", again, err)
	}

	missing := filepath.Join(t.TempDir(), "missing.db")
	inspection, err = store.Inspect(ctx, missing)
	if err != nil || inspection.Exists {
		t.Fatalf("missing = %+v, %v", inspection, err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Error("Inspect created the missing database")
	}
}

func TestInspectRejectsCorruptionAndChangedMigrations(t *testing.T) {
	damaged := seededDB(t, "event")
	corrupt(t, damaged)
	if _, err := store.Inspect(ctx, damaged); err == nil {
		t.Error("Inspect accepted a corrupt database")
	}

	changed := seededDB(t)
	db, err := store.OpenRaw(changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := store.Inspect(ctx, changed); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("err = %v, want a changed-migration error", err)
	}
}

func TestEmbeddedMigrationsAreValid(t *testing.T) {
	n, err := store.EmbeddedMigrations()
	if err != nil || n < 6 || n != store.LatestSchemaVersion() {
		t.Fatalf("EmbeddedMigrations = %d, %v", n, err)
	}
}

func TestLockDatabaseIsExclusiveAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webpty.db")
	release, err := store.LockDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LockDatabase(path); !errors.Is(err, store.ErrDatabaseInUse) {
		t.Fatalf("second lock err = %v, want ErrDatabaseInUse", err)
	}
	info, err := os.Stat(path + ".lock")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file = %v, %v; want mode 600", info, err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	again, err := store.LockDatabase(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = again()
}
