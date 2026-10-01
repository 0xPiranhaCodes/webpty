package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/store"
)

var accessEpoch = time.Unix(1_700_000_000, 0)

func tokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func createRunningTerminal(t *testing.T, s *store.Store, id string) {
	t.Helper()
	if err := s.CreateTerminalSession(context.Background(), store.TerminalSession{
		PublicID: id, State: store.TerminalRunning, Command: "/bin/sh", Rows: 24, Cols: 80,
		CreatedAt: accessEpoch, LastActivityAt: accessEpoch,
	}); err != nil {
		t.Fatal(err)
	}
}

func newGrant(id, terminal string, role store.AccessRole) store.AccessGrant {
	return store.AccessGrant{
		PublicID: id, TerminalID: terminal, Role: role, MaxRedemptions: 5,
		CreatedAt: accessEpoch, UpdatedAt: accessEpoch, ExpiresAt: accessEpoch.Add(time.Hour),
	}
}

func insertGrant(t *testing.T, s *store.Store, grant store.AccessGrant, token string) {
	t.Helper()
	err := s.WithTx(context.Background(), func(tx *store.Tx) error {
		return tx.CreateAccessGrant(context.Background(), grant, tokenHash(token))
	})
	if err != nil {
		t.Fatalf("CreateAccessGrant(%s): %v", grant.PublicID, err)
	}
}

func redeem(s *store.Store, token string, now time.Time) (store.AccessGrant, error) {
	var grant store.AccessGrant
	err := s.WithTx(context.Background(), func(tx *store.Tx) error {
		var err error
		grant, err = tx.RedeemAccessGrant(context.Background(), tokenHash(token), now)
		return err
	})
	return grant, err
}

func TestAccessGrantRoundTripNeverReturnsHash(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	createRunningTerminal(t, s, "term")

	grant := newGrant("g1", "term", store.AccessViewer)
	grant.Label = "pairing"
	insertGrant(t, s, grant, "secret-token")

	grants, err := s.AccessGrants(ctx, "term")
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("grants = %+v", grants)
	}
	got := grants[0]
	if got.PublicID != "g1" || got.TerminalID != "term" || got.Role != store.AccessViewer || got.Label != "pairing" ||
		got.MaxRedemptions != 5 || got.RedemptionCount != 0 || !got.ExpiresAt.Equal(grant.ExpiresAt) ||
		!got.RevokedAt.IsZero() || got.RevokeReason != "" {
		t.Fatalf("grant = %+v", got)
	}
	if other, err := s.AccessGrants(ctx, "other"); err != nil || len(other) != 0 {
		t.Fatalf("other terminal grants = %+v, %v", other, err)
	}

	var stored []byte
	if err := s.DB().QueryRow(`SELECT token_hash FROM access_grants`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, tokenHash("secret-token")) {
		t.Fatal("token hash not persisted as SHA-256")
	}
	var raw int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM access_grants WHERE CAST(token_hash AS TEXT) = 'secret-token'`).Scan(&raw); err != nil || raw != 0 {
		t.Fatalf("raw token persisted: %d %v", raw, err)
	}
}

func TestAccessGrantRejectsUnknownTerminalAndBadHash(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	err := s.WithTx(ctx, func(tx *store.Tx) error {
		return tx.CreateAccessGrant(ctx, newGrant("g1", "missing", store.AccessViewer), tokenHash("t"))
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown terminal err = %v, want ErrNotFound", err)
	}
	createRunningTerminal(t, s, "term")
	err = s.WithTx(ctx, func(tx *store.Tx) error {
		return tx.CreateAccessGrant(ctx, newGrant("g1", "term", store.AccessViewer), []byte("short"))
	})
	if err == nil {
		t.Fatal("short token hash accepted")
	}
	single := newGrant("g2", "term", store.AccessViewer)
	single.SingleUse = true
	err = s.WithTx(ctx, func(tx *store.Tx) error { return tx.CreateAccessGrant(ctx, single, tokenHash("t2")) })
	if err == nil {
		t.Fatal("single-use grant with max redemptions 5 accepted")
	}
}

func TestAccessGrantsAllowOnlyOneActiveEditor(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	createRunningTerminal(t, s, "term")
	insertGrant(t, s, newGrant("e1", "term", store.AccessEditor), "e1")
	err := s.WithTx(ctx, func(tx *store.Tx) error {
		return tx.CreateAccessGrant(ctx, newGrant("e2", "term", store.AccessEditor), tokenHash("e2"))
	})
	if err == nil {
		t.Fatal("second active editor accepted without revoking the first")
	}
	insertGrant(t, s, newGrant("v1", "term", store.AccessViewer), "v1")
	insertGrant(t, s, newGrant("v2", "term", store.AccessViewer), "v2")
}

func TestRedeemAccessGrantEnforcesExpiryRevocationAndLimit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	createRunningTerminal(t, s, "term")

	limited := newGrant("limited", "term", store.AccessViewer)
	limited.MaxRedemptions = 2
	insertGrant(t, s, limited, "limited")
	now := accessEpoch.Add(time.Minute)
	for i := 1; i <= 2; i++ {
		got, err := redeem(s, "limited", now)
		if err != nil {
			t.Fatalf("redeem %d: %v", i, err)
		}
		if got.RedemptionCount != i || !got.RedeemedAt.Equal(now) || got.PublicID != "limited" {
			t.Fatalf("redeem %d = %+v", i, got)
		}
	}
	if _, err := redeem(s, "limited", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("redeem past limit err = %v, want ErrNotFound", err)
	}

	insertGrant(t, s, newGrant("expiring", "term", store.AccessViewer), "expiring")
	if _, err := redeem(s, "expiring", accessEpoch.Add(time.Hour)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("redeem at expiry err = %v, want ErrNotFound", err)
	}

	insertGrant(t, s, newGrant("revoked", "term", store.AccessViewer), "revoked")
	err := s.WithTx(ctx, func(tx *store.Tx) error {
		_, _, err := tx.RevokeAccessGrant(ctx, "term", "revoked", now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := redeem(s, "revoked", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("redeem revoked err = %v, want ErrNotFound", err)
	}
	if _, err := redeem(s, "unknown", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("redeem unknown err = %v, want ErrNotFound", err)
	}
}

func TestRedeemSingleUseGrantRace(t *testing.T) {
	s := openTestStore(t)
	createRunningTerminal(t, s, "term")
	grant := newGrant("once", "term", store.AccessEditor)
	grant.SingleUse, grant.MaxRedemptions = true, 1
	insertGrant(t, s, grant, "once")

	const racers = 16
	var wg sync.WaitGroup
	results := make(chan error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := redeem(s, "once", accessEpoch.Add(time.Second))
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, store.ErrNotFound):
			t.Errorf("unexpected redeem error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("single-use grant redeemed %d times, want 1", wins)
	}
}

func TestAccessSessionLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	createRunningTerminal(t, s, "term")
	insertGrant(t, s, newGrant("g1", "term", store.AccessEditor), "invite")

	now := accessEpoch.Add(time.Minute)
	var id int64
	err := s.WithTx(ctx, func(tx *store.Tx) error {
		var err error
		id, err = tx.CreateAccessSession(ctx, tokenHash("session"), "g1", now, now.Add(10*time.Minute))
		return err
	})
	if err != nil || id == 0 {
		t.Fatalf("CreateAccessSession = %d, %v", id, err)
	}

	got, err := s.AccessSession(ctx, tokenHash("session"), now)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.Grant.PublicID != "g1" || got.Grant.TerminalID != "term" || got.Grant.Role != store.AccessEditor ||
		!got.CreatedAt.Equal(now) || !got.ExpiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("session = %+v", got)
	}
	if _, err := s.AccessSession(ctx, tokenHash("session"), now.Add(10*time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired session err = %v, want ErrNotFound", err)
	}
	if _, err := s.AccessSession(ctx, tokenHash("other"), now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown session err = %v", err)
	}

	revoked, err := s.RevokeAccessSession(ctx, tokenHash("session"), now)
	if err != nil || revoked.ID != id || revoked.Grant.PublicID != "g1" {
		t.Fatalf("RevokeAccessSession = %+v, %v", revoked, err)
	}
	if _, err := s.AccessSession(ctx, tokenHash("session"), now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked session err = %v, want ErrNotFound", err)
	}
	if _, err := s.RevokeAccessSession(ctx, tokenHash("session"), now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second revoke err = %v, want ErrNotFound", err)
	}
	var revokedAt *int64
	if err := s.DB().QueryRow(`SELECT revoked_at FROM access_sessions WHERE id = ?`, id).Scan(&revokedAt); err != nil || revokedAt == nil {
		t.Fatalf("revoked_at = %v, %v", revokedAt, err)
	}
	if _, err := s.DB().Exec(`UPDATE access_sessions SET revoked_at = NULL WHERE id = ?`, id); err == nil {
		t.Fatal("revoked session was un-revoked")
	}
}

func TestAccessSessionIsBoundToLiveGrantInStorage(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	createRunningTerminal(t, s, "term")
	insertGrant(t, s, newGrant("g1", "term", store.AccessViewer), "invite")
	now := accessEpoch.Add(time.Minute)

	create := func(token string, createdAt, expiresAt time.Time) error {
		return s.WithTx(ctx, func(tx *store.Tx) error {
			_, err := tx.CreateAccessSession(ctx, tokenHash(token), "g1", createdAt, expiresAt)
			return err
		})
	}
	if err := create("outlives", now, accessEpoch.Add(2*time.Hour)); err == nil {
		t.Fatal("session outliving its grant accepted")
	}
	if err := create("after-expiry", accessEpoch.Add(time.Hour), accessEpoch.Add(time.Hour+time.Second)); err == nil {
		t.Fatal("session created for an expired grant accepted")
	}
	if err := create("live", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	// Grant expiry and revocation end the session even though its own
	// expiry has not passed.
	if _, err := s.AccessSession(ctx, tokenHash("live"), now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	var revoked []store.AccessGrant
	err := s.WithTx(ctx, func(tx *store.Tx) error {
		grant, changed, err := tx.RevokeAccessGrant(ctx, "term", "g1", now)
		if changed {
			revoked = append(revoked, grant)
		}
		return err
	})
	if err != nil || len(revoked) != 1 || revoked[0].RevokeReason != "revoked" || !revoked[0].RevokedAt.Equal(now) {
		t.Fatalf("revoke = %+v, %v", revoked, err)
	}
	if _, err := s.AccessSession(ctx, tokenHash("live"), now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session after grant revocation err = %v, want ErrNotFound", err)
	}
	var liveSessions int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM access_sessions WHERE revoked_at IS NULL`).Scan(&liveSessions); err != nil || liveSessions != 0 {
		t.Fatalf("unrevoked sessions after grant revocation = %d, %v", liveSessions, err)
	}
	if err := create("late", now, now.Add(time.Minute)); err == nil {
		t.Fatal("session created for a revoked grant accepted")
	}
	if _, err := s.DB().Exec(`UPDATE access_grants SET revoked_at = NULL, revoke_reason = '' WHERE public_id = 'g1'`); err == nil {
		t.Fatal("revoked grant was un-revoked")
	}
}

func TestRevokeAccessGrantIsScopedAndIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	createRunningTerminal(t, s, "a")
	createRunningTerminal(t, s, "b")
	insertGrant(t, s, newGrant("ga", "a", store.AccessViewer), "ga")
	now := accessEpoch.Add(time.Minute)

	revoke := func(terminal, grant string, at time.Time) (store.AccessGrant, bool, error) {
		var got store.AccessGrant
		var changed bool
		err := s.WithTx(ctx, func(tx *store.Tx) error {
			var err error
			got, changed, err = tx.RevokeAccessGrant(ctx, terminal, grant, at)
			return err
		})
		return got, changed, err
	}
	if _, _, err := revoke("b", "ga", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoke via wrong terminal err = %v, want ErrNotFound", err)
	}
	if _, _, err := revoke("a", "missing", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoke unknown err = %v", err)
	}
	first, changed, err := revoke("a", "ga", now)
	if err != nil || !changed || !first.RevokedAt.Equal(now) || !first.UpdatedAt.Equal(now) {
		t.Fatalf("first revoke = %+v changed=%v err=%v", first, changed, err)
	}
	second, changed, err := revoke("a", "ga", now.Add(time.Minute))
	if err != nil || changed || !second.RevokedAt.Equal(now) {
		t.Fatalf("second revoke = %+v changed=%v err=%v", second, changed, err)
	}
}

func TestRevokeActiveEditorGrantsRevokesTheirSessions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	createRunningTerminal(t, s, "term")
	createRunningTerminal(t, s, "other")
	insertGrant(t, s, newGrant("e1", "term", store.AccessEditor), "e1")
	insertGrant(t, s, newGrant("v1", "term", store.AccessViewer), "v1")
	insertGrant(t, s, newGrant("eo", "other", store.AccessEditor), "eo")
	now := accessEpoch.Add(time.Minute)
	for token, grant := range map[string]string{"s-e1": "e1", "s-v1": "v1", "s-eo": "eo"} {
		if err := s.WithTx(ctx, func(tx *store.Tx) error {
			_, err := tx.CreateAccessSession(ctx, tokenHash(token), grant, now, now.Add(time.Minute))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	var replaced []store.AccessGrant
	err := s.WithTx(ctx, func(tx *store.Tx) error {
		var err error
		replaced, err = tx.RevokeActiveEditorGrants(ctx, "term", now)
		if err != nil {
			return err
		}
		return tx.CreateAccessGrant(ctx, newGrant("e2", "term", store.AccessEditor), tokenHash("e2"))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced) != 1 || replaced[0].PublicID != "e1" || replaced[0].RevokeReason != "replaced" {
		t.Fatalf("replaced = %+v", replaced)
	}
	if _, err := s.AccessSession(ctx, tokenHash("s-e1"), now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("replaced editor session err = %v, want ErrNotFound", err)
	}
	for _, token := range []string{"s-v1", "s-eo"} {
		if _, err := s.AccessSession(ctx, tokenHash(token), now); err != nil {
			t.Fatalf("unrelated session %s: %v", token, err)
		}
	}
}

func TestEditorReplacementRaceLeavesExactlyOneActiveEditor(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	createRunningTerminal(t, s, "term")

	const racers = 12
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs <- s.WithTx(ctx, func(tx *store.Tx) error {
				if _, err := tx.RevokeActiveEditorGrants(ctx, "term", accessEpoch); err != nil {
					return err
				}
				id := fmt.Sprintf("e%d", i)
				return tx.CreateAccessGrant(ctx, newGrant(id, "term", store.AccessEditor), tokenHash(id))
			})
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("replacement failed: %v", err)
		}
	}
	grants, err := s.AccessGrants(ctx, "term")
	if err != nil {
		t.Fatal(err)
	}
	active, replaced := 0, 0
	for _, g := range grants {
		switch {
		case g.RevokedAt.IsZero():
			active++
		case g.RevokeReason == "replaced":
			replaced++
		}
	}
	if len(grants) != racers || active != 1 || replaced != racers-1 {
		t.Fatalf("grants=%d active=%d replaced=%d", len(grants), active, replaced)
	}
}

func TestAccessMigrationPreservesGrantsAndDependents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "webpty.db")
	if err := store.MigrateTo(ctx, path, 3); err != nil {
		t.Fatal(err)
	}
	raw, err := store.OpenRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO terminal_sessions (id, public_id, state, created_at, last_activity_at) VALUES (7, 'old', 'running', 100, 100)`,
		`INSERT INTO access_grants (id, terminal_session_id, token_hash, role, created_at, expires_at) VALUES (3, 7, zeroblob(32), 'editor', 10, 20)`,
		`INSERT INTO access_grants (id, terminal_session_id, token_hash, role, created_at, expires_at, revoked_at) VALUES (4, 7, randomblob(32), 'viewer', 10, 20, 15)`,
		`INSERT INTO recordings (terminal_session_id, started_at) VALUES (7, 1)`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	raw.Close()

	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open after seeding: %v", err)
	}
	defer s.Close()
	grants, err := s.AccessGrants(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 2 {
		t.Fatalf("grants = %+v", grants)
	}
	active, revoked := grants[0], grants[1]
	if active.Role != store.AccessEditor || active.PublicID == "" || !active.CreatedAt.Equal(time.Unix(0, 10)) ||
		!active.UpdatedAt.Equal(time.Unix(0, 10)) || !active.ExpiresAt.Equal(time.Unix(0, 20)) || !active.RevokedAt.IsZero() ||
		active.RedemptionCount != 0 || active.MaxRedemptions < 1 {
		t.Fatalf("migrated active grant = %+v", active)
	}
	if revoked.PublicID == "" || revoked.PublicID == active.PublicID || !revoked.RevokedAt.Equal(time.Unix(0, 15)) ||
		revoked.RevokeReason != "revoked" {
		t.Fatalf("migrated revoked grant = %+v", revoked)
	}
	var recordings, foreignKeys int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM recordings`).Scan(&recordings); err != nil || recordings != 1 {
		t.Fatalf("recordings = %d, %v", recordings, err)
	}
	if err := s.DB().QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, %v", foreignKeys, err)
	}
	// The one-active-editor constraint survives the rebuild.
	if _, err := s.DB().Exec(`INSERT INTO access_grants (public_id, terminal_session_id, token_hash, role, max_redemptions,
		created_at, updated_at, expires_at) VALUES ('dup', 7, randomblob(32), 'editor', 1, 10, 10, 20)`); err == nil {
		t.Fatal("second active editor accepted after migration")
	}
	// Access sessions cascade with their grant and terminal.
	if _, err := s.DB().Exec(`INSERT INTO access_sessions (token_hash, grant_id, created_at, expires_at) VALUES (randomblob(32), 3, 11, 19)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DELETE FROM terminal_sessions WHERE id = 7`); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := s.DB().QueryRow(`SELECT (SELECT COUNT(*) FROM access_grants) + (SELECT COUNT(*) FROM access_sessions)`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("dependents after terminal delete = %d, %v", remaining, err)
	}
}
