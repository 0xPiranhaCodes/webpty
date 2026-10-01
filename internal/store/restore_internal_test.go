package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A restore whose swapped-in database fails its final check puts the
// original back, byte for byte, and keeps the rollback copy.
func TestRestoreRollsBackWhenTheSwappedDatabaseFailsVerification(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := filepath.Join(dir, "webpty.db")
	s, err := Open(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAuditEvent(ctx, AuditEvent{OccurredAt: time.Now(), Type: "original"}); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if _, err := Backup(ctx, target, backup, BackupOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAuditEvent(ctx, AuditEvent{OccurredAt: time.Now(), Type: "newer"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected verification failure")
	afterRestoreSwap = func(string) error { return injected }
	t.Cleanup(func() { afterRestoreSwap = nil })

	_, restoreErr := Restore(ctx, backup, target)
	if !errors.Is(restoreErr, injected) {
		t.Fatalf("err = %v, want the injected failure", restoreErr)
	}
	if !strings.Contains(restoreErr.Error(), ".pre-restore-") {
		t.Errorf("err = %v, want it to name the rollback copy", restoreErr)
	}
	now, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(now, original) {
		t.Fatal("the original database was not put back")
	}
	names, _ := os.ReadDir(dir)
	for _, entry := range names {
		if strings.Contains(entry.Name(), ".restore-") {
			t.Errorf("staging file %s left behind", entry.Name())
		}
	}
}
