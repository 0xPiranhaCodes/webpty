package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/0xPiranhaCodes/webpty/internal/access"
	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/collab"
	"github.com/0xPiranhaCodes/webpty/internal/httpapi"
	"github.com/0xPiranhaCodes/webpty/internal/pty"
	"github.com/0xPiranhaCodes/webpty/internal/pty/ptytest"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

const accessCookie = "webpty_access"

// hubTimers runs presence expiry timers when the test advances the clock.
type hubTimers struct {
	now    func() time.Time
	mu     sync.Mutex
	timers []*hubTimer
}

type hubTimer struct {
	at   time.Time
	f    func()
	done bool
}

func (t *hubTimer) Stop() bool { active := !t.done; t.done = true; return active }

func (ht *hubTimers) AfterFunc(d time.Duration, f func()) collab.Timer {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	timer := &hubTimer{at: ht.now().Add(d), f: f}
	ht.timers = append(ht.timers, timer)
	return timer
}

func (ht *hubTimers) fireDue() {
	ht.mu.Lock()
	var due []*hubTimer
	for _, timer := range ht.timers {
		if !timer.done && !timer.at.After(ht.now()) {
			timer.done = true
			due = append(due, timer)
		}
	}
	ht.mu.Unlock()
	for _, timer := range due {
		timer.f()
	}
}

type collabHarness struct {
	*terminalHarness
	access *access.Service
	hub    *collab.Hub
	timers *hubTimers
	store  *store.Store
}

func newCollabAPI(t *testing.T, configure func(*session.Config), settings httpapi.TerminalSettings) *collabHarness {
	t.Helper()
	starter := ptytest.NewStarter()
	h := &collabHarness{}
	security := httpapi.SecuritySettings{}
	api := newAPIWith(t, security, func(service *auth.Service, s *store.Store) []httpapi.Option {
		h.timers = &hubTimers{now: service.Now}
		h.hub = collab.NewHub(collab.Config{Now: service.Now, AfterFunc: h.timers.AfterFunc})
		t.Cleanup(h.hub.Close)
		accessService, err := access.NewService(access.Config{Store: s, Listener: h.hub, Now: service.Now, SessionTTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		h.access, h.store = accessService, s
		settings.Access, settings.Presence = accessService, h.hub
		cfg := session.Config{
			Store:          s,
			Starter:        starter,
			DefaultCommand: pty.Command{Path: "/bin/default"},
			KillGrace:      50 * time.Millisecond,
			Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
			OnEnd:          h.hub.EndTerminal,
		}
		if configure != nil {
			configure(&cfg)
		}
		m, err := session.NewManager(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
			defer cancel()
			_ = m.Close(ctx)
		})
		h.terminalHarness = &terminalHarness{manager: m, starter: starter}
		return []httpapi.Option{httpapi.WithTerminals(service, security, m, settings)}
	}, func() *access.Service { return h.access })
	h.terminalHarness.apiHarness = api
	h.cookie = h.initialize()
	_, state := h.sessionState(h.cookie)
	h.csrf, _ = state["csrfToken"].(string)
	return h
}

type invitation struct {
	grant  map[string]any
	token  string
	invite string
}

func (h *collabHarness) createGrant(terminal, body string) invitation {
	h.t.Helper()
	response := h.admin(http.MethodPost, "/api/v1/admin/terminals/"+terminal+"/grants", body)
	if response.Code != http.StatusCreated {
		h.t.Fatalf("create grant = %d %s", response.Code, response.Body)
	}
	decoded := decodeJSON(h.t, response)
	grant, _ := decoded["grant"].(map[string]any)
	token, _ := decoded["token"].(string)
	invite, _ := decoded["inviteUrl"].(string)
	return invitation{grant: grant, token: token, invite: invite}
}

func (h *collabHarness) redeem(token string) (*httptest.ResponseRecorder, *http.Cookie) {
	h.t.Helper()
	response := h.do(request{method: http.MethodPost, path: "/api/v1/access/redeem", body: `{"token":` + quote(token) + `}`, origin: testOrigin})
	if response.Code != http.StatusOK {
		return response, nil
	}
	return response, findCookie(h.t, response, accessCookie)
}

func (h *collabHarness) guest(terminal, role string) (*http.Cookie, string) {
	h.t.Helper()
	inv := h.createGrant(terminal, `{"role":"`+role+`"}`)
	response, cookie := h.redeem(inv.token)
	if cookie == nil {
		h.t.Fatalf("redeem = %d %s", response.Code, response.Body)
	}
	return cookie, inv.grant["id"].(string)
}

func (h *collabHarness) guestState(cookie *http.Cookie) (int, map[string]any) {
	h.t.Helper()
	response := h.do(request{method: http.MethodGet, path: "/api/v1/access/session", cookies: []*http.Cookie{cookie}})
	if response.Code != http.StatusOK {
		return response.Code, nil
	}
	return response.Code, decodeJSON(h.t, response)
}

func (h *collabHarness) dialAs(server *httptest.Server, id string, cookie *http.Cookie) *wsClient {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, wsURL(server, id, ""), dialOptions(server, cookie, server.URL))
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		h.t.Fatalf("dial: %v (status %d)", err, status)
	}
	h.t.Cleanup(func() { conn.CloseNow() })
	return &wsClient{t: h.t, conn: conn}
}

func dialStatus(t *testing.T, server *httptest.Server, id string, cookie *http.Cookie) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, wsURL(server, id, ""), dialOptions(server, cookie, server.URL))
	if err == nil {
		conn.CloseNow()
		return http.StatusSwitchingProtocols
	}
	if response == nil {
		t.Fatalf("dial: %v", err)
	}
	return response.StatusCode
}

func (h *collabHarness) auditCount(eventType string) int {
	h.t.Helper()
	events, err := h.store.AuditEvents(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == eventType {
			n++
		}
	}
	return n
}

func secretForms(values ...string) []string {
	var out []string
	for _, v := range values {
		sum := sha256.Sum256([]byte(v))
		out = append(out, v, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(sum[:]),
			base64.RawURLEncoding.EncodeToString(sum[:]))
	}
	return out
}

func assertNoSecrets(t *testing.T, what, body string, secrets []string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(body, secret) {
			t.Fatalf("%s leaks a secret: %s", what, body)
		}
	}
}

// --- Admin grant REST ---

func TestGrantRoutesRequireAdminHostOriginAndCSRF(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{})
	created, _ := h.createTerminal(`{}`)
	id := created["id"].(string)
	inv := h.createGrant(id, `{"role":"viewer"}`)
	grantPath := "/api/v1/admin/terminals/" + id + "/grants"
	revokePath := grantPath + "/" + inv.grant["id"].(string)

	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, grantPath, `{"role":"viewer"}`},
		{http.MethodGet, grantPath, ""},
		{http.MethodDelete, revokePath, ""},
	} {
		name := route.method + " " + route.path
		if got := h.do(request{method: route.method, path: route.path, body: route.body, origin: testOrigin}); got.Code != http.StatusUnauthorized {
			t.Errorf("%s without session = %d", name, got.Code)
		}
		if got := h.do(request{method: route.method, path: route.path, body: route.body, origin: testOrigin, csrf: h.csrf,
			cookies: []*http.Cookie{h.cookie}, host: "evil.example:8000"}); got.Code != http.StatusForbidden {
			t.Errorf("%s foreign host = %d", name, got.Code)
		}
	}
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, grantPath, `{"role":"viewer"}`},
		{http.MethodDelete, revokePath, ""},
	} {
		name := route.method + " " + route.path
		cookies := []*http.Cookie{h.cookie}
		if got := h.do(request{method: route.method, path: route.path, body: route.body, origin: testOrigin, cookies: cookies}); got.Code != http.StatusForbidden {
			t.Errorf("%s without CSRF = %d", name, got.Code)
		}
		if got := h.do(request{method: route.method, path: route.path, body: route.body, origin: "http://evil.example", csrf: h.csrf, cookies: cookies}); got.Code != http.StatusForbidden {
			t.Errorf("%s cross-origin = %d", name, got.Code)
		}
	}
	grants := decodeJSON(t, h.admin(http.MethodGet, grantPath, ""))["grants"].([]any)
	if len(grants) != 1 || grants[0].(map[string]any)["status"] != "active" {
		t.Fatalf("rejected requests changed grants: %v", grants)
	}
}

func TestCreateGrantReturnsTokenOnceAndListNeverExposesSecrets(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{})
	created, _ := h.createTerminal(`{"command":"/bin/cat","args":["--secret-flag"]}`)
	id := created["id"].(string)

	response := h.admin(http.MethodPost, "/api/v1/admin/terminals/"+id+"/grants",
		`{"role":"editor","label":"pairing","ttlSeconds":600,"singleUse":true}`)
	if response.Code != http.StatusCreated || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create = %d headers %v", response.Code, response.Header())
	}
	body := decodeJSON(t, response)
	token, _ := body["token"].(string)
	if raw, err := base64.RawURLEncoding.DecodeString(token); err != nil || len(raw) < 32 {
		t.Fatalf("token %q is not 256 random bits", token)
	}
	if body["inviteUrl"] != testOrigin+"/join#token="+token {
		t.Fatalf("inviteUrl = %v", body["inviteUrl"])
	}
	grant := body["grant"].(map[string]any)
	if grant["role"] != "editor" || grant["label"] != "pairing" || grant["singleUse"] != true || grant["maxRedemptions"] != float64(1) ||
		grant["status"] != "active" || grant["terminalId"] != id || grant["redemptionCount"] != float64(0) {
		t.Fatalf("grant = %v", grant)
	}
	expires, _ := time.Parse(time.RFC3339, grant["expiresAt"].(string))
	if !expires.Equal(h.clock.Now().Add(10 * time.Minute)) {
		t.Fatalf("expiresAt = %v", grant["expiresAt"])
	}

	list := h.admin(http.MethodGet, "/api/v1/admin/terminals/"+id+"/grants", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list = %d", list.Code)
	}
	assertNoSecrets(t, "grant list", list.Body.String(), secretForms(token))
	if strings.Contains(list.Body.String(), "token") || strings.Contains(list.Body.String(), "hash") {
		t.Fatalf("grant list mentions tokens: %s", list.Body)
	}
	grants := decodeJSON(t, list)["grants"].([]any)
	if len(grants) != 1 || grants[0].(map[string]any)["id"] != grant["id"] {
		t.Fatalf("grants = %v", grants)
	}

	revoked := h.admin(http.MethodDelete, "/api/v1/admin/terminals/"+id+"/grants/"+grant["id"].(string), "")
	if revoked.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", revoked.Code, revoked.Body)
	}
	assertNoSecrets(t, "revoke response", revoked.Body.String(), secretForms(token))
	if got := decodeJSON(t, revoked); got["status"] != "revoked" || got["revokedAt"] == nil {
		t.Fatalf("revoked grant = %v", got)
	}
	if again := h.admin(http.MethodDelete, "/api/v1/admin/terminals/"+id+"/grants/"+grant["id"].(string), ""); again.Code != http.StatusOK {
		t.Fatalf("second revoke = %d", again.Code)
	}
	if h.auditCount("access.grant.revoked") != 1 {
		t.Fatal("repeated revoke audited twice")
	}

	events, _ := h.store.AuditEvents(context.Background())
	encoded, _ := json.Marshal(events)
	assertNoSecrets(t, "audit log", string(encoded), secretForms(token))
}

func TestCreateGrantValidationAndErrors(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	path := "/api/v1/admin/terminals/" + id + "/grants"
	for name, body := range map[string]string{
		"owner role":     `{"role":"owner"}`,
		"missing role":   `{}`,
		"unknown field":  `{"role":"viewer","admin":true}`,
		"negative ttl":   `{"role":"viewer","ttlSeconds":-5}`,
		"huge ttl":       `{"role":"viewer","ttlSeconds":99999999999999}`,
		"short ttl":      `{"role":"viewer","ttlSeconds":1}`,
		"many uses":      `{"role":"viewer","maxRedemptions":1000000}`,
		"trailing data":  `{"role":"viewer"} {}`,
		"control label":  `{"role":"viewer","label":"a\u0007"}`,
		"single and max": `{"role":"viewer","singleUse":true,"maxRedemptions":3}`,
	} {
		if response := h.admin(http.MethodPost, path, body); response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d body %s, want 400", name, response.Code, response.Body)
		}
	}
	assertErrorCode(t, h.admin(http.MethodPost, "/api/v1/admin/terminals/missing/grants", `{"role":"viewer"}`), http.StatusNotFound, "not_found")
	assertErrorCode(t, h.admin(http.MethodGet, "/api/v1/admin/terminals/missing/grants", ""), http.StatusNotFound, "not_found")
	assertErrorCode(t, h.admin(http.MethodDelete, path+"/missing", ""), http.StatusNotFound, "not_found")

	other, _ := h.createTerminal(`{}`)
	inv := h.createGrant(other["id"].(string), `{"role":"viewer"}`)
	assertErrorCode(t, h.admin(http.MethodDelete, path+"/"+inv.grant["id"].(string), ""), http.StatusNotFound, "not_found")

	p.Exit(pty.ExitStatus{Code: 0})
	eventually(t, "exit", func() bool { return h.state(id) == store.TerminalExited })
	assertErrorCode(t, h.admin(http.MethodPost, path, `{"role":"viewer"}`), http.StatusConflict, "invalid_state")
}

// --- Redemption and guest sessions ---

func TestRedeemIssuesSeparateStrictCookie(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{})
	created, _ := h.createTerminal(`{"command":"/bin/cat","args":["--secret-flag"]}`)
	id := created["id"].(string)
	inv := h.createGrant(id, `{"role":"viewer","singleUse":true}`)

	response, cookie := h.redeem(inv.token)
	if cookie == nil {
		t.Fatalf("redeem = %d %s", response.Code, response.Body)
	}
	if response.Header().Get("Referrer-Policy") != "no-referrer" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("redeem headers = %v", response.Header())
	}
	assertSecureCookieAttributes(t, cookie, false)
	if cookie.Value == inv.token || cookie.Value == "" || cookie.MaxAge <= 0 {
		t.Fatalf("access cookie = %+v, must be a separate session token", cookie)
	}
	for _, header := range response.Header().Values("Set-Cookie") {
		if strings.Contains(header, inv.token) {
			t.Fatal("invite token set as a cookie")
		}
	}
	body := decodeJSON(t, response)
	terminal, _ := body["terminal"].(map[string]any)
	if body["role"] != "viewer" || terminal["id"] != id || terminal["state"] != "running" || body["csrfToken"] == "" {
		t.Fatalf("redeem body = %v", body)
	}
	for _, field := range []string{"command", "args", "failure"} {
		if _, ok := terminal[field]; ok {
			t.Fatalf("guest terminal metadata exposes %s: %v", field, terminal)
		}
	}
	if body["permissions"].(map[string]any)["input"] != false {
		t.Fatalf("viewer permissions = %v", body["permissions"])
	}

	again, _ := h.redeem(inv.token)
	assertErrorCode(t, again, http.StatusUnauthorized, "invalid_invitation")
	if again.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("failed redemption lacks Referrer-Policy")
	}
	bogus, _ := h.redeem("bogus")
	assertErrorCode(t, bogus, http.StatusUnauthorized, "invalid_invitation")

	fresh := h.createGrant(id, `{"role":"viewer"}`)
	for name, r := range map[string]request{
		"cross origin":  {method: http.MethodPost, path: "/api/v1/access/redeem", body: `{"token":` + quote(fresh.token) + `}`, origin: "http://evil.example"},
		"no origin":     {method: http.MethodPost, path: "/api/v1/access/redeem", body: `{"token":` + quote(fresh.token) + `}`},
		"foreign host":  {method: http.MethodPost, path: "/api/v1/access/redeem", body: `{"token":` + quote(fresh.token) + `}`, origin: testOrigin, host: "evil.example:8000"},
		"unknown field": {method: http.MethodPost, path: "/api/v1/access/redeem", body: `{"token":` + quote(fresh.token) + `,"role":"editor"}`, origin: testOrigin},
		"text body":     {method: http.MethodPost, path: "/api/v1/access/redeem", body: fresh.token, origin: testOrigin, headers: map[string]string{"Content-Type": "text/plain"}},
	} {
		if got := h.do(r); got.Code < 400 || got.Code >= 500 || got.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: status = %d headers %v", name, got.Code, got.Header())
		}
	}
	if grants := decodeJSON(t, h.admin(http.MethodGet, "/api/v1/admin/terminals/"+id+"/grants", ""))["grants"].([]any); grants[1].(map[string]any)["redemptionCount"] != float64(0) {
		t.Fatalf("rejected redemptions consumed the grant: %v", grants[1])
	}
}

func TestAccessSessionAndLogout(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{})
	created, _ := h.createTerminal(`{}`)
	id := created["id"].(string)
	cookie, grantID := h.guest(id, "editor")

	status, state := h.guestState(cookie)
	if status != http.StatusOK || state["role"] != "editor" || state["grantId"] != grantID ||
		state["terminal"].(map[string]any)["id"] != id || state["permissions"].(map[string]any)["input"] != true {
		t.Fatalf("session = %d %v", status, state)
	}
	csrf, _ := state["csrfToken"].(string)
	if csrf == "" || csrf == h.csrf {
		t.Fatalf("csrf = %q", csrf)
	}
	if status, _ := h.guestState(&http.Cookie{Name: accessCookie, Value: "forged"}); status != http.StatusUnauthorized {
		t.Fatalf("forged session = %d", status)
	}

	logout := func(r request) int {
		r.method, r.path = http.MethodPost, "/api/v1/access/logout"
		return h.do(r).Code
	}
	cookies := []*http.Cookie{cookie}
	if got := logout(request{origin: testOrigin, cookies: cookies}); got != http.StatusForbidden {
		t.Errorf("logout without CSRF = %d", got)
	}
	if got := logout(request{origin: testOrigin, cookies: cookies, csrf: h.csrf}); got != http.StatusForbidden {
		t.Errorf("logout with admin CSRF = %d", got)
	}
	if got := logout(request{origin: "http://evil.example", cookies: cookies, csrf: csrf}); got != http.StatusForbidden {
		t.Errorf("cross-origin logout = %d", got)
	}
	if status, _ := h.guestState(cookie); status != http.StatusOK {
		t.Fatal("rejected logouts ended the session")
	}
	response := h.do(request{method: http.MethodPost, path: "/api/v1/access/logout", origin: testOrigin, cookies: cookies, csrf: csrf})
	if response.Code != http.StatusNoContent {
		t.Fatalf("logout = %d", response.Code)
	}
	if cleared := findCookie(t, response, accessCookie); cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Fatalf("cookie not cleared: %+v", cleared)
	}
	if status, _ := h.guestState(cookie); status != http.StatusUnauthorized {
		t.Fatalf("session after logout = %d", status)
	}
	if h.auditCount("access.logout") != 1 {
		t.Fatal("logout not audited")
	}
}

func TestGuestSessionsCannotReachAdminEndpoints(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	cookie, grantID := h.guest(id, "editor")
	_, state := h.guestState(cookie)
	guestCSRF := state["csrfToken"].(string)

	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/admin/session", ""},
		{http.MethodPost, "/api/v1/admin/logout", ""},
		{http.MethodPost, "/api/v1/admin/password", `{"currentPassword":"x","newPassword":"yyyyyyyyyyyyyy"}`},
		{http.MethodGet, "/api/v1/admin/terminals", ""},
		{http.MethodPost, "/api/v1/admin/terminals", `{}`},
		{http.MethodGet, "/api/v1/admin/terminals/" + id, ""},
		{http.MethodDelete, "/api/v1/admin/terminals/" + id, ""},
		{http.MethodGet, "/api/v1/admin/terminals/" + id + "/grants", ""},
		{http.MethodPost, "/api/v1/admin/terminals/" + id + "/grants", `{"role":"editor"}`},
		{http.MethodDelete, "/api/v1/admin/terminals/" + id + "/grants/" + grantID, ""},
	} {
		for _, csrf := range []string{"", guestCSRF} {
			response := h.do(request{method: route.method, path: route.path, body: route.body, origin: testOrigin, csrf: csrf, cookies: []*http.Cookie{cookie}})
			if response.Code != http.StatusUnauthorized {
				t.Errorf("guest %s %s = %d, want 401", route.method, route.path, response.Code)
			}
		}
	}
	if p.Exited() {
		t.Fatal("guest terminated the terminal")
	}
	if status, _ := h.guestState(cookie); status != http.StatusOK {
		t.Fatal("guest session changed")
	}
}

// --- WebSocket authorization ---

func TestWebSocketAcceptsAccessSessionsOnlyForTheirTerminal(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{})
	a, _ := h.createTerminal(`{}`)
	b, _ := h.createTerminal(`{}`)
	server := h.server()
	cookie, grantID := h.guest(a["id"].(string), "viewer")

	if got := dialStatus(t, server, b["id"].(string), cookie); got != http.StatusForbidden {
		t.Errorf("wrong terminal = %d, want 403", got)
	}
	if got := dialStatus(t, server, "missing", cookie); got != http.StatusForbidden {
		t.Errorf("unknown terminal = %d, want 403", got)
	}
	if got := dialStatus(t, server, a["id"].(string), &http.Cookie{Name: accessCookie, Value: "forged"}); got != http.StatusUnauthorized {
		t.Errorf("forged cookie = %d, want 401", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
	defer cancel()
	header := http.Header{"Cookie": {accessCookie + "=" + cookie.Value}, "Origin": {"http://evil.example"}}
	if _, response, err := websocket.Dial(ctx, wsURL(server, a["id"].(string), ""), &websocket.DialOptions{
		HTTPHeader: header, Subprotocols: []string{httpapi.TerminalProtocol}}); err == nil || response.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin guest dial err=%v", err)
	}

	client := h.dialAs(server, a["id"].(string), cookie)
	ready := client.readType("ready")
	if ready["role"] != "viewer" {
		t.Fatalf("ready = %v", ready)
	}

	if response := h.admin(http.MethodDelete, "/api/v1/admin/terminals/"+a["id"].(string)+"/grants/"+grantID, ""); response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
	if got := dialStatus(t, server, a["id"].(string), cookie); got != http.StatusUnauthorized {
		t.Errorf("revoked session = %d, want 401", got)
	}
}

func TestWebSocketEnforcesRolesForEveryAction(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{})
	created, p := h.createTerminal(`{"command":"/bin/cat","args":["--secret-flag"]}`)
	id := created["id"].(string)
	server := h.server()
	editorCookie, _ := h.guest(id, "editor")
	viewerCookie, _ := h.guest(id, "viewer")

	owner := h.dialAs(server, id, h.cookie)
	if ready := owner.readType("ready"); ready["role"] != "owner" || ready["session"].(map[string]any)["command"] != "/bin/cat" {
		t.Fatalf("owner ready = %v", ready)
	}
	editor := h.dialAs(server, id, editorCookie)
	editorReady := editor.readType("ready")
	if editorReady["role"] != "editor" || editorReady["permissions"].(map[string]any)["input"] != true {
		t.Fatalf("editor ready = %v", editorReady)
	}
	viewer := h.dialAs(server, id, viewerCookie)
	viewerReady := viewer.readType("ready")
	if viewerReady["role"] != "viewer" || viewerReady["permissions"].(map[string]any)["input"] != false {
		t.Fatalf("viewer ready = %v", viewerReady)
	}
	for _, field := range []string{"command", "args"} {
		if _, ok := viewerReady["session"].(map[string]any)[field]; ok {
			t.Fatalf("viewer ready exposes %s", field)
		}
	}

	viewer.send(`{"type":"input","data":"rm -rf /\r"}`)
	if msg := viewer.readType("error"); msg["code"] != "permission_denied" || msg["action"] != "input" {
		t.Fatalf("viewer input = %v", msg)
	}
	viewer.send(`{"type":"input","data":"again"}`)
	viewer.readType("error")
	viewer.send(`{"type":"resize","rows":5,"cols":5}`)
	if msg := viewer.readType("error"); msg["code"] != "permission_denied" || msg["action"] != "resize" {
		t.Fatalf("viewer resize = %v", msg)
	}
	viewer.send(`{"type":"ping"}`)
	viewer.readType("pong")

	editor.send(`{"type":"input","data":"e"}`)
	editor.send(`{"type":"resize","rows":30,"cols":90}`)
	editor.send(`{"type":"ping"}`)
	editor.readType("pong")
	owner.send(`{"type":"input","data":"o"}`)
	owner.send(`{"type":"ping"}`)
	owner.readType("pong")
	eventually(t, "owner and editor input", func() bool { return p.Input() == "eo" })
	for _, size := range p.Sizes() {
		if size == (pty.Size{Rows: 5, Cols: 5}) {
			t.Fatal("viewer resize reached the PTY")
		}
	}
	if sizes := p.Sizes(); sizes[len(sizes)-1] != (pty.Size{Rows: 30, Cols: 90}) {
		t.Fatalf("editor resize not applied: %v", sizes)
	}
	if n := h.auditCount("terminal.input.denied"); n != 2 {
		t.Fatalf("denied audit events = %d, want one per denied action", n)
	}
	if viewer.conn.Subprotocol() != httpapi.TerminalProtocol {
		t.Fatal("viewer lost the protocol")
	}
}

func TestEditorReplacementRevokesLiveEditorBeforeResponse(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{PingInterval: time.Hour})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()
	oldCookie, _ := h.guest(id, "editor")
	owner := h.dialAs(server, id, h.cookie)
	owner.readType("ready")
	owner.readType("presence_snapshot")
	editor := h.dialAs(server, id, oldCookie)
	editor.readType("ready")
	snapshot := editor.readType("presence_snapshot")
	editorID := snapshot["self"].(string)
	owner.readType("participant_joined")

	next := h.createGrant(id, `{"role":"editor"}`)
	// The replaced editor already lost input permission when the grant
	// creation returned, without waiting for the heartbeat.
	editor.send(`{"type":"input","data":"late"}`)

	// The denial of the late input and the permission_changed event are
	// written by different goroutines, so either may arrive first.
	var changed map[string]any
	for changed == nil {
		msg := editor.read()
		switch {
		case msg["type"] == "permission_changed":
			changed = msg
		case msg["type"] == "error" && msg["code"] == "permission_denied":
		default:
			t.Fatalf("unexpected message before permission_changed: %v", msg)
		}
	}
	participant := changed["participant"].(map[string]any)
	if participant["id"] != editorID || changed["reason"] != "replaced" || changed["permissions"].(map[string]any)["input"] != false {
		t.Fatalf("editor permission_changed = %v", changed)
	}
	for {
		_, data, err := editor.conn.Read(context.Background())
		if err != nil {
			if status := websocket.CloseStatus(err); status != httpapi.CloseUnauthorized {
				t.Fatalf("replaced editor close = %v (%v)", status, err)
			}
			break
		}
		var msg map[string]any
		_ = json.Unmarshal(data, &msg)
		if msg["type"] != "error" || msg["code"] != "permission_denied" {
			t.Fatalf("unexpected message after replacement: %s", data)
		}
	}
	if changed := owner.readType("permission_changed"); changed["participant"].(map[string]any)["id"] != editorID {
		t.Fatalf("owner permission_changed = %v", changed)
	}
	if left := owner.readType("participant_left"); left["participant"].(map[string]any)["id"] != editorID || left["reason"] != "replaced" {
		t.Fatalf("owner participant_left = %v", left)
	}
	owner.send(`{"type":"ping"}`)
	owner.readType("pong")
	if strings.Contains(p.Input(), "late") {
		t.Fatal("replaced editor's input reached the PTY")
	}
	if status, _ := h.guestState(oldCookie); status != http.StatusUnauthorized {
		t.Fatalf("replaced editor session = %d", status)
	}
	if got := dialStatus(t, server, id, oldCookie); got != http.StatusUnauthorized {
		t.Fatalf("replaced editor redial = %d", got)
	}

	_, newCookie := h.redeem(next.token)
	fresh := h.dialAs(server, id, newCookie)
	fresh.readType("ready")
	fresh.send(`{"type":"input","data":"new"}`)
	eventually(t, "new editor input", func() bool { return strings.Contains(p.Input(), "new") })
}

func TestRevokingViewerGrantClosesItsConnectionImmediately(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{PingInterval: time.Hour})
	created, _ := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()
	cookie, grantID := h.guest(id, "viewer")
	viewer := h.dialAs(server, id, cookie)
	viewer.readType("ready")
	viewer.readType("presence_snapshot")

	start := time.Now()
	if response := h.admin(http.MethodDelete, "/api/v1/admin/terminals/"+id+"/grants/"+grantID, ""); response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
	if changed := viewer.readType("permission_changed"); changed["reason"] != "revoked" {
		t.Fatalf("permission_changed = %v", changed)
	}
	if code, reason := viewer.closeStatus(); code != httpapi.CloseUnauthorized || !strings.Contains(reason, "revoked") {
		t.Fatalf("close = %v %q", code, reason)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("revocation waited for the heartbeat")
	}
}

func TestGuestLogoutClosesItsConnections(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{PingInterval: time.Hour})
	created, _ := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()
	cookie, _ := h.guest(id, "viewer")
	other, _ := h.guest(id, "viewer")
	client := h.dialAs(server, id, cookie)
	client.readType("ready")
	bystander := h.dialAs(server, id, other)
	bystander.readType("ready")
	_, state := h.guestState(cookie)

	response := h.do(request{method: http.MethodPost, path: "/api/v1/access/logout", origin: testOrigin,
		cookies: []*http.Cookie{cookie}, csrf: state["csrfToken"].(string)})
	if response.Code != http.StatusNoContent {
		t.Fatal(response.Code)
	}
	if code, _ := client.closeStatus(); code != httpapi.CloseUnauthorized {
		t.Fatalf("close = %v", code)
	}
	bystander.send(`{"type":"ping"}`)
	bystander.readType("pong")
}

// manualHeartbeat lets a test decide exactly when heartbeats happen.
func manualHeartbeat() (chan time.Time, func(time.Duration) (<-chan time.Time, func())) {
	ticks := make(chan time.Time)
	return ticks, func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }
}

func TestHeartbeatRevalidatesGuestOnEachTickAndNotBefore(t *testing.T) {
	ticks, heartbeat := manualHeartbeat()
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{PingInterval: time.Hour, Heartbeat: heartbeat})
	created, _ := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()
	cookie, _ := h.guest(id, "viewer")
	client := h.dialAs(server, id, cookie)
	client.readType("ready")

	ticks <- time.Now()
	client.send(`{"type":"ping"}`)
	client.readType("pong")

	if _, err := h.store.RevokeAccessSession(context.Background(), access.HashToken(cookie.Value), h.access.Now()); err != nil {
		t.Fatal(err)
	}
	client.send(`{"type":"ping"}`)
	client.readType("pong")

	ticks <- time.Now()
	if code, _ := client.closeStatus(); code != httpapi.CloseUnauthorized {
		t.Fatalf("close = %v, want %v", code, httpapi.CloseUnauthorized)
	}
}

func TestHeartbeatClosesOwnerOnceTheAdminSessionExpires(t *testing.T) {
	ticks, heartbeat := manualHeartbeat()
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{PingInterval: time.Hour, Heartbeat: heartbeat})
	created, _ := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()
	owner := h.dial(server, id, "")
	owner.readType("ready")

	h.clock.Advance(13 * time.Hour)
	owner.send(`{"type":"ping"}`)
	owner.readType("pong")

	ticks <- time.Now()
	if code, _ := owner.closeStatus(); code != httpapi.CloseUnauthorized {
		t.Fatalf("close = %v, want %v", code, httpapi.CloseUnauthorized)
	}
}

func TestAccessExpiryIsEnforcedPerActionAndClosesWithoutHeartbeat(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{PingInterval: time.Hour})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()
	inv := h.createGrant(id, `{"role":"editor","ttlSeconds":60}`)
	_, cookie := h.redeem(inv.token)
	editor := h.dialAs(server, id, cookie)
	editor.readType("ready")
	// The snapshot comes from the presence loop, so it may arrive after the
	// input loop's error reply; take it here before expecting other presence messages.
	editor.readType("presence_snapshot")
	editor.send(`{"type":"input","data":"a"}`)
	eventually(t, "input before expiry", func() bool { return p.Input() == "a" })

	h.clock.Advance(time.Minute)
	editor.send(`{"type":"input","data":"b"}`)
	if msg := editor.readType("error"); msg["code"] != "permission_denied" {
		t.Fatalf("input after expiry = %v", msg)
	}
	h.timers.fireDue()
	if changed := editor.readType("permission_changed"); changed["reason"] != "expired" {
		t.Fatalf("permission_changed = %v", changed)
	}
	if code, _ := editor.closeStatus(); code != httpapi.CloseUnauthorized {
		t.Fatalf("close = %v", code)
	}
	if p.Input() != "a" {
		t.Fatalf("input after expiry reached the PTY: %q", p.Input())
	}
}

// --- Presence ---

func TestPresenceOverWebSocket(t *testing.T) {
	h := newCollabAPI(t, nil, httpapi.TerminalSettings{})
	created, p := h.createTerminal(`{}`)
	id := created["id"].(string)
	server := h.server()
	cookie, _ := h.guest(id, "viewer")

	var raw []string
	record := func(c *wsClient, want string) map[string]any {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
		defer cancel()
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		raw = append(raw, string(data))
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil || msg["type"] != want {
			t.Fatalf("message %s, want type %q", data, want)
		}
		return msg
	}

	owner := h.dialAs(server, id, h.cookie)
	record(owner, "ready")
	ownerSnapshot := record(owner, "presence_snapshot")
	ownerID := ownerSnapshot["self"].(string)
	if ps := ownerSnapshot["participants"].([]any); len(ps) != 1 || ps[0].(map[string]any)["role"] != "owner" {
		t.Fatalf("owner snapshot = %v", ownerSnapshot)
	}

	viewer := h.dialAs(server, id, cookie)
	record(viewer, "ready")
	viewerSnapshot := record(viewer, "presence_snapshot")
	viewerID := viewerSnapshot["self"].(string)
	if len(viewerSnapshot["participants"].([]any)) != 2 || viewerID == ownerID {
		t.Fatalf("viewer snapshot = %v", viewerSnapshot)
	}
	joined := record(owner, "participant_joined")
	if who := joined["participant"].(map[string]any); who["id"] != viewerID || who["role"] != "viewer" ||
		joined["version"] != viewerSnapshot["version"] {
		t.Fatalf("joined = %v", joined)
	}

	viewer.conn.CloseNow()
	if left := record(owner, "participant_left"); left["participant"].(map[string]any)["id"] != viewerID || left["reason"] != "disconnected" {
		t.Fatalf("left = %v", left)
	}
	again := h.dialAs(server, id, cookie)
	record(again, "ready")
	againSnapshot := record(again, "presence_snapshot")
	rejoined := record(owner, "participant_joined")
	newID := rejoined["participant"].(map[string]any)["id"]
	if newID == viewerID || againSnapshot["self"] != newID || len(againSnapshot["participants"].([]any)) != 2 {
		t.Fatalf("reconnect: joined %v snapshot %v", rejoined, againSnapshot)
	}
	if stats := h.hub.Stats(); stats.Participants != 2 {
		t.Fatalf("hub stats = %+v, want stale participant removed", stats)
	}

	secrets := append(secretForms(cookie.Value, h.cookie.Value), "192.0.2.", "127.0.0.1", "::1")
	for _, message := range raw {
		if !strings.Contains(message, `"presence_snapshot"`) && !strings.Contains(message, `"participant_`) {
			continue
		}
		assertNoSecrets(t, "presence message", message, secrets)
	}
	if h.auditCount("access.participant.joined") != 3 || h.auditCount("access.participant.left") != 1 {
		t.Fatalf("participant audit joined=%d left=%d", h.auditCount("access.participant.joined"), h.auditCount("access.participant.left"))
	}

	p.Exit(pty.ExitStatus{Code: 0})
	owner.readType("exit")
	eventually(t, "presence cleanup after terminal end", func() bool { return h.hub.Stats() == collab.Stats{} })
}
