package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/0xPiranhaCodes/webpty/internal/httpapi"
)

func TestAdminTerminalJSONCountsLiveParticipants(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{})
	created, _ := h.createTerminal(`{}`)
	id := created["id"].(string)
	if created["participants"] != float64(0) {
		t.Fatalf("created participants = %v, want 0", created["participants"])
	}

	server := h.server()
	owner := h.dialAs(server, id, h.cookie)
	cookie, _ := h.guest(id, "viewer")
	viewer := h.dialAs(server, id, cookie)
	for _, c := range []*wsClient{owner, viewer} {
		ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
		for range 2 {
			if _, _, err := c.conn.Read(ctx); err != nil {
				t.Fatal(err)
			}
		}
		cancel()
	}

	got := decodeJSON(t, h.admin(http.MethodGet, "/api/v1/admin/terminals/"+id, ""))
	if got["participants"] != float64(2) {
		t.Fatalf("get participants = %v, want 2", got["participants"])
	}
	list := decodeJSON(t, h.admin(http.MethodGet, "/api/v1/admin/terminals", ""))
	sessions := list["sessions"].([]any)
	if len(sessions) != 1 || sessions[0].(map[string]any)["participants"] != float64(2) {
		t.Fatalf("list = %v", sessions)
	}

	guest := h.do(request{method: http.MethodGet, path: "/api/v1/access/session", cookies: []*http.Cookie{cookie}})
	if terminal := decodeJSON(t, guest)["terminal"].(map[string]any); terminal["participants"] != nil {
		t.Fatalf("guests see participant counts: %v", terminal)
	}
}
