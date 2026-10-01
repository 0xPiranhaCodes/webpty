package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/httpapi"
)

const secureOrigin = "https://webpty.example"

func TestSecureCookiesUseHostPrefix(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{PublicOrigin: secureOrigin, SecureCookies: true})
	response := h.do(request{method: http.MethodPost, path: "/api/v1/admin/login", body: `{"password":"CHANGEME"}`,
		origin: secureOrigin, host: "webpty.example"})
	if response.Code != http.StatusOK {
		t.Fatalf("login = %d %s", response.Code, response.Body)
	}
	bootstrap := findCookie(t, response, "__Host-webpty_bootstrap")
	assertSecureCookieAttributes(t, bootstrap, true)
	if bootstrap.Domain != "" {
		t.Fatalf("__Host- cookie has Domain %q", bootstrap.Domain)
	}
	for _, c := range response.Result().Cookies() {
		if !strings.HasPrefix(c.Name, "__Host-") {
			t.Fatalf("secure mode set unprefixed cookie %s", c.Name)
		}
	}
	ok := h.do(request{method: http.MethodGet, path: "/api/v1/admin/session", host: "webpty.example", cookies: []*http.Cookie{bootstrap}})
	if ok.Code != http.StatusOK {
		t.Fatalf("session with prefixed cookie = %d", ok.Code)
	}
	// An unprefixed cookie can be planted by a network attacker over HTTP;
	// secure mode must ignore it.
	planted := &http.Cookie{Name: "webpty_bootstrap", Value: bootstrap.Value}
	if got := h.do(request{method: http.MethodGet, path: "/api/v1/admin/session", host: "webpty.example", cookies: []*http.Cookie{planted}}); got.Code != http.StatusUnauthorized {
		t.Fatalf("session with unprefixed cookie in secure mode = %d, want 401", got.Code)
	}
}

func TestHTTPDevelopmentCookiesStayUnprefixed(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	response := h.login("CHANGEME")
	cookie := findCookie(t, response, "webpty_bootstrap")
	assertSecureCookieAttributes(t, cookie, false)
}

func TestAdminLogoutRevokesEverySuppliedSession(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{})
	created, _ := h.createTerminal(`{}`)
	guestCookie, _ := h.guest(created["id"].(string), "viewer")

	response := h.do(request{method: http.MethodPost, path: "/api/v1/admin/logout", origin: testOrigin, csrf: h.csrf,
		cookies: []*http.Cookie{h.cookie, guestCookie}})
	if response.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s", response.Code, response.Body)
	}
	if code, _ := h.sessionState(h.cookie); code != http.StatusUnauthorized {
		t.Fatalf("admin session after logout = %d", code)
	}
	if code, _ := h.guestState(guestCookie); code != http.StatusUnauthorized {
		t.Fatalf("access session supplied with admin logout = %d, want 401", code)
	}
	cleared := map[string]bool{}
	for _, c := range response.Result().Cookies() {
		if c.MaxAge < 0 {
			cleared[c.Name] = true
		}
	}
	for _, name := range []string{"webpty_session", "webpty_bootstrap", accessCookie} {
		if !cleared[name] {
			t.Errorf("logout did not clear %s; Set-Cookie = %v", name, response.Header().Values("Set-Cookie"))
		}
	}
}

func TestBootstrapCookieDoesNotBlockGuestTerminal(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{})
	created, _ := h.createTerminal(`{}`)
	id := created["id"].(string)
	guestCookie, _ := h.guest(id, "viewer")
	// A stale browser can still hold a bootstrap session: reset the
	// credential so one can be issued alongside the guest session.
	if _, err := h.store.DB().Exec(`DELETE FROM admin_credentials`); err != nil {
		t.Fatal(err)
	}
	bootstrap := h.bootstrapCookie()

	server := h.server()
	header := http.Header{}
	header.Set("Cookie", bootstrap.Name+"="+bootstrap.Value+"; "+guestCookie.Name+"="+guestCookie.Value)
	header.Set("Origin", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, wsURL(server, id, ""), &websocket.DialOptions{
		HTTPHeader: header, Subprotocols: []string{httpapi.TerminalProtocol}})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("guest dial with a bootstrap cookie: %v (status %d)", err, status)
	}
	defer conn.CloseNow()
	ready := (&wsClient{t: t, conn: conn}).readType("ready")
	if ready["role"] != "viewer" {
		t.Fatalf("ready role = %v, want viewer", ready["role"])
	}
}

func TestRedeemIsThrottledPerClientAndAuditedOnce(t *testing.T) {
	var h *collabHarness
	now := func() time.Time { return h.clock.Now() }
	h = newCollabAPI(t, nil, httpapi.TerminalSettings{
		RedeemThrottle: auth.NewThrottle(auth.ThrottleConfig{Limit: 3, Window: time.Minute, Now: now}),
	})
	created, _ := h.createTerminal(`{}`)
	inv := h.createGrant(created["id"].(string), `{"role":"viewer"}`)
	bogus := strings.Repeat("A", 43)
	for i := range 3 {
		if response, _ := h.redeem(bogus); response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d", i+1, response.Code)
		}
	}
	for range 5 {
		response, _ := h.redeem(inv.token)
		if response.Code != http.StatusTooManyRequests {
			t.Fatalf("throttled redeem = %d %s", response.Code, response.Body)
		}
		if retry, _ := strconv.Atoi(response.Header().Get("Retry-After")); retry != 60 {
			t.Fatalf("Retry-After = %q", response.Header().Get("Retry-After"))
		}
	}
	h.clock.Advance(30 * time.Second)
	if response, _ := h.redeem(inv.token); response.Header().Get("Retry-After") != "30" {
		t.Fatalf("Retry-After 30s into the window = %q, want 30", response.Header().Get("Retry-After"))
	}
	if n := h.auditCount("access.redeem.throttled"); n != 1 {
		t.Fatalf("throttle audit events = %d, want 1", n)
	}
	h.clock.Advance(time.Minute)
	if response, cookie := h.redeem(inv.token); cookie == nil {
		t.Fatalf("redeem after the window = %d %s", response.Code, response.Body)
	}
}

func TestRedeemRevokesTheBrowsersPriorAccessSession(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{})
	created, _ := h.createTerminal(`{}`)
	id := created["id"].(string)
	first, _ := h.guest(id, "viewer")
	second := h.createGrant(id, `{"role":"viewer"}`)

	response := h.do(request{method: http.MethodPost, path: "/api/v1/access/redeem", body: `{"token":` + quote(second.token) + `}`,
		origin: testOrigin, cookies: []*http.Cookie{first}})
	if response.Code != http.StatusOK {
		t.Fatalf("redeem = %d %s", response.Code, response.Body)
	}
	replacement := findCookie(t, response, accessCookie)
	if code, _ := h.guestState(first); code != http.StatusUnauthorized {
		t.Fatalf("prior access session = %d, want 401", code)
	}
	if code, _ := h.guestState(replacement); code != http.StatusOK {
		t.Fatalf("new access session = %d", code)
	}
	if n := h.auditCount("access.session.superseded"); n != 1 {
		t.Fatalf("superseded audit events = %d, want 1", n)
	}
}

func TestFailedUpgradeIsNeverAnnounced(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{PingInterval: time.Hour})
	created, _ := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()
	owner := h.dial(server, id, "")
	owner.readType("ready")
	owner.readType("presence_snapshot")
	guestCookie, _ := h.guest(id, "viewer")

	// Authorized and offering the protocol, but not a WebSocket handshake.
	response := h.do(request{method: http.MethodGet, path: "/api/v1/terminals/" + id + "/ws", origin: testOrigin,
		cookies: []*http.Cookie{guestCookie}, headers: map[string]string{"Sec-WebSocket-Protocol": httpapi.TerminalProtocol}})
	if response.Code == http.StatusSwitchingProtocols {
		t.Fatal("plain request was upgraded")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, data, err := owner.conn.Read(ctx); err == nil {
		t.Fatalf("owner was told about a participant that never connected: %s", data)
	}
	if n := h.hub.ParticipantCount(id); n != 1 {
		t.Fatalf("participants = %d, want only the owner", n)
	}
}

func TestOwnerResizeIsSentToViewers(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{PingInterval: time.Hour})
	created, _ := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()
	owner := h.dial(server, id, "")
	owner.readType("ready")
	guestCookie, _ := h.guest(id, "viewer")
	viewer := h.dialAs(server, id, guestCookie)
	viewer.readType("ready")

	owner.send(`{"type":"resize","rows":33,"cols":101}`)
	msg := viewer.readType("resize")
	if msg["rows"] != float64(33) || msg["cols"] != float64(101) {
		t.Fatalf("viewer resize = %v", msg)
	}
}

func TestSingleInputLargerThanQueueBudgetIsDelivered(t *testing.T) {
	h := newTerminalAPI(t, nil, httpapi.TerminalSettings{InputQueueBytes: 16})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	client := h.dial(h.server(), id, "")
	client.readType("ready")

	long := strings.Repeat("x", 40)
	client.send(`{"type":"input","data":"` + long + `"}`)
	client.send(`{"type":"ping"}`)
	client.readType("pong")
	eventually(t, "oversized input", func() bool { return p.Input() == long })
}

func TestRecordingListPagesWithNextCursor(t *testing.T) {
	h := newRecordingAPI(t, nil)
	for i := 0; i < 3; i++ {
		h.createTerminal(`{}`)
	}
	list := func(query string) ([]any, any) {
		response := h.admin(http.MethodGet, "/api/v1/admin/recordings?"+query, "")
		if response.Code != http.StatusOK {
			t.Fatalf("list %s = %d %s", query, response.Code, response.Body)
		}
		body := decodeJSON(t, response)
		recs, _ := body["recordings"].([]any)
		return recs, body["nextCursor"]
	}
	eventually(t, "three recordings", func() bool { recs, _ := list("limit=10"); return len(recs) == 3 })

	first, cursor := list("limit=2")
	if len(first) != 2 || cursor != first[1].(map[string]any)["id"] {
		t.Fatalf("first page = %v, cursor %v", first, cursor)
	}
	second, end := list("limit=2&before=" + cursor.(string))
	if len(second) != 1 || end != nil {
		t.Fatalf("second page = %v, cursor %v", second, end)
	}
	assertErrorCode(t, h.admin(http.MethodGet, "/api/v1/admin/recordings?before=nope", ""), http.StatusBadRequest, "invalid_argument")
}

// deadlineRecorder is a ResponseWriter whose write deadline can be set, as
// on a real connection.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadlines = append(d.deadlines, deadline)
	return nil
}

// A long export extends its write deadline as it streams, so the server's
// whole-response WriteTimeout cannot truncate it.
func TestExportExtendsItsWriteDeadlineWhileStreaming(t *testing.T) {
	h := newRecordingAPI(t, nil)
	_, rec := h.recordedTerminal()
	req := httptest.NewRequest(http.MethodGet, testOrigin+"/api/v1/admin/recordings/"+rec["id"].(string)+"/export", nil)
	req.RemoteAddr = "192.0.2.1:54321"
	req.AddCookie(h.cookie)
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	start := time.Now()
	h.handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"o"`) {
		t.Fatalf("export = %d %q", w.Code, w.Body)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.deadlines) == 0 {
		t.Fatal("export never extended its write deadline")
	}
	for _, deadline := range w.deadlines {
		if !deadline.IsZero() && deadline.Before(start) {
			t.Fatalf("deadline %v is in the past", deadline)
		}
	}
}
