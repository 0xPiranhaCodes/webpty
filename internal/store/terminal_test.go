package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/store"
)

func exitCode(code int) *int { return &code }

func TestTerminalSessionRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	created := time.Unix(1_700_000_000, 0)

	session := store.TerminalSession{
		PublicID:       "term-1",
		State:          store.TerminalStarting,
		Command:        "/bin/sh",
		Args:           []string{"-c", "echo hi"},
		Rows:           24,
		Cols:           80,
		CreatedAt:      created,
		LastActivityAt: created,
	}
	if err := s.CreateTerminalSession(ctx, session); err != nil {
		t.Fatalf("CreateTerminalSession: %v", err)
	}
	if err := s.CreateTerminalSession(ctx, session); err == nil {
		t.Fatal("duplicate public id accepted")
	}

	got, err := s.TerminalSession(ctx, "term-1")
	if err != nil {
		t.Fatalf("TerminalSession: %v", err)
	}
	if !reflect.DeepEqual(got, session) {
		t.Fatalf("TerminalSession = %+v, want %+v", got, session)
	}

	session.State = store.TerminalExited
	session.Rows, session.Cols = 40, 120
	session.StartedAt = created.Add(time.Second)
	session.EndedAt = created.Add(time.Minute)
	session.LastActivityAt = created.Add(30 * time.Second)
	session.ExitCode = exitCode(3)
	session.ExitSignal = ""
	if err := s.UpdateTerminalSession(ctx, session); err != nil {
		t.Fatalf("UpdateTerminalSession: %v", err)
	}
	got, err = s.TerminalSession(ctx, "term-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, session) {
		t.Fatalf("after update = %+v, want %+v", got, session)
	}
}

func TestTerminalSessionAcceptsEveryLifecycleState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, state := range []store.TerminalState{
		store.TerminalStarting, store.TerminalRunning, store.TerminalExited, store.TerminalFailed, store.TerminalTerminated,
	} {
		err := s.CreateTerminalSession(ctx, store.TerminalSession{
			PublicID: string(state), State: state, Command: "/bin/sh", Rows: 24, Cols: 80,
			CreatedAt: time.Unix(1, 0), LastActivityAt: time.Unix(1, 0),
		})
		if err != nil {
			t.Errorf("state %q rejected: %v", state, err)
		}
	}
	err := s.CreateTerminalSession(ctx, store.TerminalSession{
		PublicID: "bogus", State: "paused", Command: "/bin/sh", Rows: 24, Cols: 80,
		CreatedAt: time.Unix(1, 0), LastActivityAt: time.Unix(1, 0),
	})
	if err == nil {
		t.Error("unknown state accepted")
	}
}

func TestTerminalSessionLookupsAndListing(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.TerminalSession(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing lookup err = %v, want ErrNotFound", err)
	}
	if err := s.UpdateTerminalSession(ctx, store.TerminalSession{PublicID: "missing", State: store.TerminalRunning, Rows: 1, Cols: 1}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing update err = %v, want ErrNotFound", err)
	}

	for _, id := range []string{"a", "b", "c"} {
		if err := s.CreateTerminalSession(ctx, store.TerminalSession{
			PublicID: id, State: store.TerminalRunning, Command: "/bin/sh", Args: []string{}, Rows: 24, Cols: 80,
			CreatedAt: time.Unix(1, 0), LastActivityAt: time.Unix(1, 0),
		}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.TerminalSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, session := range list {
		ids = append(ids, session.PublicID)
	}
	if !reflect.DeepEqual(ids, []string{"a", "b", "c"}) {
		t.Fatalf("ids = %v, want creation order", ids)
	}
}

func TestFailInterruptedTerminalSessions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for id, state := range map[string]store.TerminalState{
		"starting": store.TerminalStarting, "running": store.TerminalRunning, "exited": store.TerminalExited,
	} {
		if err := s.CreateTerminalSession(ctx, store.TerminalSession{
			PublicID: id, State: state, Command: "/bin/sh", Rows: 24, Cols: 80,
			CreatedAt: time.Unix(1, 0), LastActivityAt: time.Unix(1, 0),
		}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Unix(50, 0)
	count, err := s.FailInterruptedTerminalSessions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
	for id, want := range map[string]store.TerminalState{
		"starting": store.TerminalFailed, "running": store.TerminalFailed, "exited": store.TerminalExited,
	} {
		got, err := s.TerminalSession(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != want {
			t.Errorf("%s state = %q, want %q", id, got.State, want)
		}
		if want == store.TerminalFailed && (!got.EndedAt.Equal(now) || got.Failure == "") {
			t.Errorf("%s endedAt = %v failure = %q, want %v and a reason", id, got.EndedAt, got.Failure, now)
		}
	}
}

func TestTerminalMigrationPreservesRowsAndDependents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "webpty.db")
	if err := store.MigrateTo(ctx, path, 2); err != nil {
		t.Fatal(err)
	}

	raw, err := store.OpenRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO terminal_sessions (id, public_id, state, created_at, ended_at, exit_code) VALUES (7, 'old', 'exited', 100, 200, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO access_grants (terminal_session_id, token_hash, role, created_at, expires_at) VALUES (7, zeroblob(32), 'viewer', 1, 2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO recordings (terminal_session_id, started_at) VALUES (7, 1)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open after seeding: %v", err)
	}
	defer s.Close()

	got, err := s.TerminalSession(ctx, "old")
	if err != nil {
		t.Fatalf("migrated row: %v", err)
	}
	if got.State != store.TerminalExited || got.ExitCode == nil || *got.ExitCode != 0 ||
		!got.CreatedAt.Equal(time.Unix(0, 100)) || !got.EndedAt.Equal(time.Unix(0, 200)) {
		t.Errorf("migrated row = %+v", got)
	}

	var grants, recordings int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM access_grants`).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM recordings`).Scan(&recordings); err != nil {
		t.Fatal(err)
	}
	if grants != 1 || recordings != 1 {
		t.Fatalf("dependents after migration: grants=%d recordings=%d, want 1 and 1", grants, recordings)
	}

	var foreignKeys int
	if err := s.DB().QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d after migration, want 1", foreignKeys)
	}
	if _, err := s.DB().Exec(`DELETE FROM terminal_sessions WHERE public_id = 'old'`); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM access_grants`).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 0 {
		t.Fatalf("grants = %d after deleting terminal, want cascade to 0", grants)
	}
}
