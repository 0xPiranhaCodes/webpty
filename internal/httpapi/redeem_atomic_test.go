package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/httpapi"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

// failingMetadata makes terminal metadata lookups fail while fail is set.
type failingMetadata struct {
	session.Repository
	fail atomic.Bool
}

func (f *failingMetadata) TerminalSession(ctx context.Context, id string) (store.TerminalSession, error) {
	if f.fail.Load() {
		return store.TerminalSession{}, errors.New("injected metadata failure")
	}
	return f.Repository.TerminalSession(ctx, id)
}

func TestRedeemMetadataFailureLeavesInvitationUnused(t *testing.T) {
	metadata := &failingMetadata{}
	h := newCollabAPI(t, func(cfg *session.Config) {
		metadata.Repository = cfg.Store
		cfg.Store = metadata
	}, httpapi.TerminalSettings{})
	created, _ := h.createTerminal(`{}`)
	id := created["id"].(string)
	inv := h.createGrant(id, `{"role":"viewer","singleUse":true}`)
	if response := h.admin(http.MethodDelete, "/api/v1/admin/terminals/"+id, ""); response.Code != http.StatusOK {
		t.Fatalf("terminate = %d", response.Code)
	}
	metadata.fail.Store(true)
	// Once the terminal has left the live set, metadata comes from the store.
	deadline := time.Now().Add(terminalWait)
	for {
		if _, err := h.manager.Get(context.Background(), id); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminal never left the live set")
		}
		time.Sleep(time.Millisecond)
	}

	response, cookie := h.redeem(inv.token)
	if response.Code != http.StatusInternalServerError || cookie != nil || len(response.Result().Cookies()) != 0 {
		t.Fatalf("redeem with failing metadata = %d, cookies %v", response.Code, response.Result().Cookies())
	}
	var sessions, redemptions int
	if err := h.store.DB().QueryRow(`SELECT count(*) FROM access_sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DB().QueryRow(`SELECT redemption_count FROM access_grants`).Scan(&redemptions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || redemptions != 0 || h.auditCount("access.grant.redeemed") != 0 {
		t.Fatalf("failed redeem left sessions=%d redemptions=%d audits=%d", sessions, redemptions, h.auditCount("access.grant.redeemed"))
	}

	metadata.fail.Store(false)
	if response, cookie := h.redeem(inv.token); response.Code != http.StatusOK || cookie == nil {
		t.Fatalf("single-use invitation unusable after the failed attempt: %d %s", response.Code, response.Body)
	}
}
