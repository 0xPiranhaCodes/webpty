package app_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/app"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// A running server holds the database lock, so offline maintenance such as
// restore, and a second server, cannot use the same database.
func TestServeHoldsTheDatabaseLockWhileRunning(t *testing.T) {
	cfg := testConfig(t)
	_, cancel, done := serveAdmin(t, cfg)

	if _, err := store.LockDatabase(cfg.DatabasePath); !errors.Is(err, store.ErrDatabaseInUse) {
		t.Fatalf("lock while serving err = %v, want ErrDatabaseInUse", err)
	}
	second, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Serve(context.Background(), cfg, second); !errors.Is(err, store.ErrDatabaseInUse) {
		t.Fatalf("second Serve = %v, want ErrDatabaseInUse", err)
	}
	if _, err := second.Accept(); err == nil {
		t.Fatal("second listener left open")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Serve did not return")
	}
	release, err := store.LockDatabase(cfg.DatabasePath)
	if err != nil {
		t.Fatalf("lock after shutdown: %v", err)
	}
	_ = release()
}
