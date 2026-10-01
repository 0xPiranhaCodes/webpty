package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "webpty.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenAppliesInitialSchema(t *testing.T) {
	s := openTestStore(t)

	for _, table := range []string{
		"settings",
		"admin_credentials",
		"admin_sessions",
		"terminal_sessions",
		"access_grants",
		"recordings",
		"recording_chunks",
		"audit_events",
	} {
		var name string
		err := s.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing: %v", table, err)
		}
	}
}

func TestOpenEnablesForeignKeysWALAndBusyTimeout(t *testing.T) {
	s := openTestStore(t)

	var foreignKeys int
	if err := s.DB().QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}

	var journalMode string
	if err := s.DB().QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var busyTimeout int
	if err := s.DB().QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if busyTimeout < 1000 {
		t.Errorf("busy_timeout = %d, want >= 1000", busyTimeout)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	s := openTestStore(t)

	_, err := s.DB().Exec(`INSERT INTO recordings (public_id, terminal_session_id, status, format_version, codec,
		started_at, rows, cols, updated_at) VALUES ('r', 999, 'recording', 1, 'gzip', 1, 24, 80, 1)`)
	if err == nil {
		t.Fatal("inserting recording for missing terminal succeeded, want foreign key error")
	}
}

func TestMigrationsAreRecordedAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webpty.db")
	ctx := context.Background()

	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var firstCount int
	if err := first.DB().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&firstCount); err != nil {
		t.Fatal(err)
	}
	if firstCount == 0 {
		t.Fatal("no migrations recorded")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	var secondCount int
	if err := second.DB().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&secondCount); err != nil {
		t.Fatal(err)
	}
	if secondCount != firstCount {
		t.Errorf("migrations recorded = %d after reopen, want %d", secondCount, firstCount)
	}
}

func TestWithTxRollsBackOnError(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	sentinel := errors.New("boom")

	err := s.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.SetAdminCredential(ctx, "hash", time.Unix(100, 0)); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx error = %v, want sentinel", err)
	}

	_, err = s.AdminCredential(ctx)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("AdminCredential after rollback err = %v, want ErrNotFound", err)
	}
}

func TestAdminCredentialRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.AdminCredential(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty store err = %v, want ErrNotFound", err)
	}

	err := s.WithTx(ctx, func(tx *store.Tx) error {
		return tx.SetAdminCredential(ctx, "first", time.Unix(100, 0))
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.WithTx(ctx, func(tx *store.Tx) error {
		return tx.SetAdminCredential(ctx, "second", time.Unix(200, 0))
	})
	if err != nil {
		t.Fatal(err)
	}

	credential, err := s.AdminCredential(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if credential.PasswordHash != "second" {
		t.Errorf("PasswordHash = %q, want second", credential.PasswordHash)
	}
	if !credential.UpdatedAt.Equal(time.Unix(200, 0)) {
		t.Errorf("UpdatedAt = %v, want %v", credential.UpdatedAt, time.Unix(200, 0))
	}
}

func TestAdminCredentialVersionIncrementsOnEveryWrite(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for want := int64(1); want <= 3; want++ {
		err := s.WithTx(ctx, func(tx *store.Tx) error {
			return tx.SetAdminCredential(ctx, "hash", time.Unix(want, 0))
		})
		if err != nil {
			t.Fatal(err)
		}
		credential, err := s.AdminCredential(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if credential.Version != want {
			t.Fatalf("Version = %d after %d writes, want %d", credential.Version, want, want)
		}
	}
}

func TestTxAdminSessionSeesLiveSessionsOnly(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Unix(1_000, 0)
	hash := []byte("tx-hash-tx-hash-tx-hash-tx-hash-")

	err := s.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.CreateAdminSession(ctx, store.AdminSession{
			TokenHash: hash, Kind: store.SessionKindAdmin, CreatedAt: now, ExpiresAt: now.Add(time.Minute),
		}); err != nil {
			return err
		}
		if _, err := tx.AdminSession(ctx, hash, now); err != nil {
			t.Errorf("live session inside tx: %v", err)
		}
		if _, err := tx.AdminSession(ctx, hash, now.Add(time.Minute)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expired session inside tx err = %v, want ErrNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAdminSessionLookupEnforcesExpiry(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Unix(1_000, 0)
	hash := []byte("0123456789abcdef0123456789abcdef")

	err := s.WithTx(ctx, func(tx *store.Tx) error {
		return tx.CreateAdminSession(ctx, store.AdminSession{
			TokenHash: hash,
			Kind:      store.SessionKindAdmin,
			CreatedAt: now,
			ExpiresAt: now.Add(time.Minute),
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	session, err := s.AdminSession(ctx, hash, now.Add(59*time.Second))
	if err != nil {
		t.Fatalf("lookup before expiry: %v", err)
	}
	if session.Kind != store.SessionKindAdmin {
		t.Errorf("Kind = %q, want admin", session.Kind)
	}

	if _, err := s.AdminSession(ctx, hash, now.Add(time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lookup at expiry err = %v, want ErrNotFound", err)
	}
}

func TestAdminSessionRevocation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Unix(1_000, 0)
	first := []byte("first-hash-first-hash-first-hash")
	second := []byte("second-hash-second-hash-second-h")

	err := s.WithTx(ctx, func(tx *store.Tx) error {
		for _, hash := range [][]byte{first, second} {
			if err := tx.CreateAdminSession(ctx, store.AdminSession{
				TokenHash: hash, Kind: store.SessionKindAdmin, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteAdminSession(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminSession(ctx, first, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked session err = %v, want ErrNotFound", err)
	}
	if _, err := s.AdminSession(ctx, second, now); err != nil {
		t.Fatalf("unrelated session revoked: %v", err)
	}

	if err := s.WithTx(ctx, func(tx *store.Tx) error { return tx.DeleteAllAdminSessions(ctx) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminSession(ctx, second, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session after delete-all err = %v, want ErrNotFound", err)
	}
}

func TestAdminSessionRejectsUnknownKind(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Unix(1_000, 0)

	err := s.WithTx(ctx, func(tx *store.Tx) error {
		return tx.CreateAdminSession(ctx, store.AdminSession{
			TokenHash: []byte("hash-hash-hash-hash-hash-hash-ha"), Kind: "root", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		})
	})
	if err == nil {
		t.Fatal("creating session with unknown kind succeeded")
	}
}

func TestAuditEventsAppendAndList(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	events := []store.AuditEvent{
		{OccurredAt: time.Unix(10, 0), Type: "admin.login.failure", RemoteAddr: "192.0.2.1", Details: map[string]string{"reason": "invalid_password"}},
		{OccurredAt: time.Unix(20, 0), Type: "admin.login.success", RemoteAddr: "192.0.2.1"},
	}
	for _, event := range events {
		if err := s.AppendAuditEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}

	listed, err := s.AuditEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("len = %d, want 2", len(listed))
	}
	if listed[0].Type != "admin.login.failure" || listed[0].Details["reason"] != "invalid_password" {
		t.Errorf("first event = %+v", listed[0])
	}
	if listed[1].Type != "admin.login.success" || !listed[1].OccurredAt.Equal(time.Unix(20, 0)) {
		t.Errorf("second event = %+v", listed[1])
	}
}
