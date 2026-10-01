package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/access"
	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/httpapi"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

const (
	testOrigin      = "http://localhost:8000"
	testNewPassword = "a much better passphrase"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type apiHarness struct {
	t       *testing.T
	handler http.Handler
	clock   *clock
	store   *store.Store
}

func newAPI(t *testing.T, security httpapi.SecuritySettings) *apiHarness {
	t.Helper()
	return newAPIWith(t, security, nil)
}

// newAPIWith mounts admin auth plus the options returned by extra, which
// share the harness store and auth service.
func newAPIWith(t *testing.T, security httpapi.SecuritySettings, extra func(*auth.Service, *store.Store) []httpapi.Option,
	guests ...func() *access.Service) *apiHarness {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "webpty.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	service := auth.NewService(auth.ServiceConfig{
		Store:            s,
		PasswordParams:   auth.PasswordParams{Memory: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32},
		SessionTTL:       time.Hour,
		BootstrapTTL:     10 * time.Minute,
		Throttle:         auth.NewThrottle(auth.ThrottleConfig{Limit: 5, Window: time.Minute, MaxEntries: 16, Now: c.Now}),
		PasswordThrottle: auth.NewThrottle(auth.ThrottleConfig{Limit: 3, Window: time.Minute, MaxEntries: 16, Now: c.Now}),
		Now:              c.Now,
	})
	var extraOptions []httpapi.Option
	if extra != nil {
		extraOptions = extra(service, s)
	}
	var guestSessions []*access.Service
	for _, guest := range guests {
		guestSessions = append(guestSessions, guest())
	}
	options := append([]httpapi.Option{httpapi.WithAdminAuth(service, security, guestSessions...)}, extraOptions...)
	// Handlers, including hijacked WebSocket ones, finish before the store
	// they write to closes.
	drain := httpapi.NewDrain()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := drain.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return &apiHarness{t: t, handler: drain.Wrap(httpapi.NewRouter(options...)), clock: c, store: s}
}

type request struct {
	method  string
	path    string
	body    string
	origin  string
	csrf    string
	cookies []*http.Cookie
	headers map[string]string
	host    string
}

func (h *apiHarness) do(r request) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(r.method, testOrigin+r.path, strings.NewReader(r.body))
	if r.host != "" {
		req.Host = r.host
	}
	req.RemoteAddr = "192.0.2.1:54321"
	if r.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.origin != "" {
		req.Header.Set("Origin", r.origin)
	}
	if r.csrf != "" {
		req.Header.Set("X-CSRF-Token", r.csrf)
	}
	for key, value := range r.headers {
		req.Header.Set(key, value)
	}
	for _, cookie := range r.cookies {
		req.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, req)
	return recorder
}

func (h *apiHarness) login(password string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(request{method: http.MethodPost, path: "/api/v1/admin/login", body: `{"password":` + quote(password) + `}`, origin: testOrigin})
}

func (h *apiHarness) bootstrapCookie() *http.Cookie {
	h.t.Helper()
	response := h.login("CHANGEME")
	if response.Code != http.StatusOK {
		h.t.Fatalf("bootstrap login status = %d", response.Code)
	}
	return findCookie(h.t, response, "webpty_bootstrap")
}

func (h *apiHarness) initialize() *http.Cookie {
	h.t.Helper()
	response := h.do(request{
		method:  http.MethodPost,
		path:    "/api/v1/admin/password",
		body:    `{"currentPassword":"CHANGEME","newPassword":` + quote(testNewPassword) + `}`,
		origin:  testOrigin,
		cookies: []*http.Cookie{h.bootstrapCookie()},
	})
	if response.Code != http.StatusNoContent {
		h.t.Fatalf("initialize status = %d body %s", response.Code, response.Body)
	}
	return findCookie(h.t, response, "webpty_session")
}

func (h *apiHarness) sessionState(cookie *http.Cookie) (int, map[string]any) {
	h.t.Helper()
	response := h.do(request{method: http.MethodGet, path: "/api/v1/admin/session", cookies: []*http.Cookie{cookie}})
	var body map[string]any
	if response.Code == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			h.t.Fatal(err)
		}
	}
	return response.Code, body
}

func quote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func findCookie(t *testing.T, response *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response has no %s cookie; Set-Cookie = %v", name, response.Header().Values("Set-Cookie"))
	return nil
}

func assertSecureCookieAttributes(t *testing.T, cookie *http.Cookie, secure bool) {
	t.Helper()
	if !cookie.HttpOnly {
		t.Errorf("%s: HttpOnly = false", cookie.Name)
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("%s: SameSite = %v, want Strict", cookie.Name, cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("%s: Path = %q, want /", cookie.Name, cookie.Path)
	}
	if cookie.Secure != secure {
		t.Errorf("%s: Secure = %v, want %v", cookie.Name, cookie.Secure, secure)
	}
}

func decodeJSON(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode %q: %v", response.Body.String(), err)
	}
	return body
}

func TestHealthzStillServedWithAdminAuth(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	if response := h.do(request{method: http.MethodGet, path: "/healthz"}); response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
}

func TestBootstrapLoginIssuesBootstrapCookie(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	response := h.login("CHANGEME")

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", response.Code, response.Body)
	}
	if body := decodeJSON(t, response); body["passwordChangeRequired"] != true {
		t.Errorf("body = %v, want passwordChangeRequired true", body)
	}
	cookie := findCookie(t, response, "webpty_bootstrap")
	assertSecureCookieAttributes(t, cookie, false)
	if cookie.MaxAge <= 0 || cookie.MaxAge > 600 {
		t.Errorf("bootstrap cookie MaxAge = %d, want short-lived (<= 600s)", cookie.MaxAge)
	}
	if cacheControl := response.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cacheControl)
	}
}

func TestSecureCookieMode(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{SecureCookies: true, PublicOrigin: "https://pty.example.com"})
	response := h.do(request{method: http.MethodPost, path: "/api/v1/admin/login", body: `{"password":"CHANGEME"}`, origin: "https://pty.example.com"})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", response.Code, response.Body)
	}
	assertSecureCookieAttributes(t, findCookie(t, response, "__Host-webpty_bootstrap"), true)
}

func TestLoginWrongPasswordIsGeneric401(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	response := h.login("wrong")

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	if cookies := response.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("cookies set on failure: %v", cookies)
	}
	if body := decodeJSON(t, response); body["error"] != "invalid credentials" {
		t.Errorf("body = %v, want generic error", body)
	}
}

func TestLoginRequestValidation(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	cases := map[string]struct {
		req  request
		want int
	}{
		"form content type": {request{method: http.MethodPost, path: "/api/v1/admin/login", body: `{"password":"CHANGEME"}`, origin: testOrigin,
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}}, http.StatusUnsupportedMediaType},
		"unknown field": {request{method: http.MethodPost, path: "/api/v1/admin/login", body: `{"password":"CHANGEME","admin":true}`, origin: testOrigin}, http.StatusBadRequest},
		"malformed":     {request{method: http.MethodPost, path: "/api/v1/admin/login", body: `{"password":`, origin: testOrigin}, http.StatusBadRequest},
		"oversized":     {request{method: http.MethodPost, path: "/api/v1/admin/login", body: `{"password":"` + strings.Repeat("x", 64*1024) + `"}`, origin: testOrigin}, http.StatusRequestEntityTooLarge},
		"cross origin":  {request{method: http.MethodPost, path: "/api/v1/admin/login", body: `{"password":"CHANGEME"}`, origin: "http://evil.test"}, http.StatusForbidden},
		"wrong method":  {request{method: http.MethodGet, path: "/api/v1/admin/login"}, http.StatusMethodNotAllowed},
	}
	for name, tc := range cases {
		if response := h.do(tc.req); response.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", name, response.Code, tc.want)
		}
	}
}

func TestSessionEndpoint(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})

	if code, _ := h.sessionState(&http.Cookie{Name: "webpty_session", Value: "forged"}); code != http.StatusUnauthorized {
		t.Errorf("forged cookie status = %d, want 401", code)
	}
	if response := h.do(request{method: http.MethodGet, path: "/api/v1/admin/session"}); response.Code != http.StatusUnauthorized {
		t.Errorf("no cookie status = %d, want 401", response.Code)
	}

	code, body := h.sessionState(h.bootstrapCookie())
	if code != http.StatusOK {
		t.Fatalf("bootstrap session status = %d", code)
	}
	if body["state"] != "bootstrap" || body["passwordChangeRequired"] != true {
		t.Errorf("bootstrap body = %v", body)
	}
	if token, _ := body["csrfToken"].(string); token == "" {
		t.Errorf("bootstrap body has no csrfToken: %v", body)
	}
}

func TestBootstrapTokenIsNotAnAdminSession(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	bootstrap := h.bootstrapCookie()

	if code, _ := h.sessionState(&http.Cookie{Name: "webpty_session", Value: bootstrap.Value}); code != http.StatusUnauthorized {
		t.Fatalf("bootstrap token as admin cookie status = %d, want 401", code)
	}
}

func TestBootstrapPasswordChange(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	bootstrap := h.bootstrapCookie()
	change := func(body, origin string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		return h.do(request{method: http.MethodPost, path: "/api/v1/admin/password", body: body, origin: origin, cookies: cookies})
	}
	valid := `{"currentPassword":"CHANGEME","newPassword":` + quote(testNewPassword) + `}`

	if response := change(valid, testOrigin); response.Code != http.StatusUnauthorized {
		t.Errorf("without cookie status = %d, want 401", response.Code)
	}
	if response := change(valid, "http://evil.test", bootstrap); response.Code != http.StatusForbidden {
		t.Errorf("cross origin status = %d, want 403", response.Code)
	}
	if response := change(valid, "", bootstrap); response.Code != http.StatusForbidden {
		t.Errorf("missing origin status = %d, want 403", response.Code)
	}
	if response := change(`{"currentPassword":"WRONG","newPassword":`+quote(testNewPassword)+`}`, testOrigin, bootstrap); response.Code != http.StatusForbidden {
		t.Errorf("wrong current password status = %d, want 403", response.Code)
	}
	for _, weak := range []string{"short", "CHANGEME", "elevenchars"} {
		if response := change(`{"currentPassword":"CHANGEME","newPassword":`+quote(weak)+`}`, testOrigin, bootstrap); response.Code != http.StatusBadRequest {
			t.Errorf("weak password %q status = %d, want 400", weak, response.Code)
		}
	}

	response := change(valid, testOrigin, bootstrap)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d body %s, want 204", response.Code, response.Body)
	}
	session := findCookie(t, response, "webpty_session")
	assertSecureCookieAttributes(t, session, false)
	if cleared := findCookie(t, response, "webpty_bootstrap"); cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Errorf("bootstrap cookie not cleared: %+v", cleared)
	}

	if code, _ := h.sessionState(bootstrap); code != http.StatusUnauthorized {
		t.Errorf("old bootstrap cookie status = %d, want 401", code)
	}
	code, body := h.sessionState(session)
	if code != http.StatusOK || body["state"] != "authenticated" || body["passwordChangeRequired"] != false {
		t.Errorf("new session = %d %v, want authenticated", code, body)
	}
}

func TestLoginAfterInitialization(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	h.initialize()

	if response := h.login("CHANGEME"); response.Code != http.StatusUnauthorized {
		t.Errorf("CHANGEME after init status = %d, want 401", response.Code)
	}
	response := h.login(testNewPassword)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if body := decodeJSON(t, response); body["passwordChangeRequired"] != false {
		t.Errorf("body = %v, want passwordChangeRequired false", body)
	}
	cookie := findCookie(t, response, "webpty_session")
	assertSecureCookieAttributes(t, cookie, false)
	if len(cookie.Value) < 40 {
		t.Errorf("session cookie value too short: %d", len(cookie.Value))
	}
}

func TestAdminMutationsRequireCSRFAndOrigin(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	session := h.initialize()
	_, state := h.sessionState(session)
	csrf, _ := state["csrfToken"].(string)
	logout := func(origin, token string) int {
		return h.do(request{method: http.MethodPost, path: "/api/v1/admin/logout", origin: origin, csrf: token, cookies: []*http.Cookie{session}}).Code
	}

	if code := logout(testOrigin, ""); code != http.StatusForbidden {
		t.Errorf("missing CSRF status = %d, want 403", code)
	}
	if code := logout(testOrigin, "not-the-token"); code != http.StatusForbidden {
		t.Errorf("wrong CSRF status = %d, want 403", code)
	}
	if code := logout("http://evil.test", csrf); code != http.StatusForbidden {
		t.Errorf("cross origin status = %d, want 403", code)
	}
	if code := logout("", csrf); code != http.StatusForbidden {
		t.Errorf("missing origin status = %d, want 403", code)
	}

	passwordChange := h.do(request{
		method: http.MethodPost, path: "/api/v1/admin/password", origin: testOrigin, cookies: []*http.Cookie{session},
		body: `{"currentPassword":` + quote(testNewPassword) + `,"newPassword":"another good passphrase"}`,
	})
	if passwordChange.Code != http.StatusForbidden {
		t.Errorf("admin password change without CSRF status = %d, want 403", passwordChange.Code)
	}

	if code, _ := h.sessionState(session); code != http.StatusOK {
		t.Fatalf("session revoked by rejected requests: %d", code)
	}
}

func TestRefererIsAcceptedWhenOriginAbsent(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	response := h.do(request{method: http.MethodPost, path: "/api/v1/admin/login", body: `{"password":"CHANGEME"}`,
		headers: map[string]string{"Referer": testOrigin + "/login"}})
	if response.Code != http.StatusOK {
		t.Fatalf("same-origin referer status = %d, want 200", response.Code)
	}
}

func TestPublicOriginOverridesHost(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{PublicOrigin: "https://pty.example.com"})
	if response := h.login("CHANGEME"); response.Code != http.StatusForbidden {
		t.Errorf("host origin with public origin configured status = %d, want 403", response.Code)
	}
	response := h.do(request{method: http.MethodPost, path: "/api/v1/admin/login", body: `{"password":"CHANGEME"}`, origin: "https://pty.example.com"})
	if response.Code != http.StatusOK {
		t.Errorf("public origin status = %d, want 200", response.Code)
	}
}

func TestLogout(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	session := h.initialize()
	_, state := h.sessionState(session)

	response := h.do(request{method: http.MethodPost, path: "/api/v1/admin/logout", origin: testOrigin, csrf: state["csrfToken"].(string), cookies: []*http.Cookie{session}})
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d body %s, want 204", response.Code, response.Body)
	}
	if cleared := findCookie(t, response, "webpty_session"); cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Errorf("session cookie not cleared: %+v", cleared)
	}
	if code, _ := h.sessionState(session); code != http.StatusUnauthorized {
		t.Errorf("session after logout status = %d, want 401", code)
	}
	if response := h.do(request{method: http.MethodPost, path: "/api/v1/admin/logout", origin: testOrigin}); response.Code != http.StatusUnauthorized {
		t.Errorf("logout without session status = %d, want 401", response.Code)
	}
}

func TestSessionExpiryOverHTTP(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	bootstrap := h.bootstrapCookie()
	h.clock.Advance(10 * time.Minute)

	if code, _ := h.sessionState(bootstrap); code != http.StatusUnauthorized {
		t.Fatalf("expired bootstrap status = %d, want 401", code)
	}
}

func TestLoginThrottleReturns429(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	for range 5 {
		h.login("wrong")
	}
	response := h.login("CHANGEME")
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.Code)
	}
	if response.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
}

func TestLoginThrottleRetryAfterIsTimeLeftInWindow(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	for range 5 {
		h.login("wrong")
	}
	h.clock.Advance(40*time.Second + 500*time.Millisecond)
	response := h.login("CHANGEME")
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.Code)
	}
	if got := response.Header().Get("Retry-After"); got != "20" {
		t.Fatalf("Retry-After = %q, want 20 (seconds left, rounded up)", got)
	}
}

func TestImplicitOriginRejectsForeignHostDuringBootstrap(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	const rebound = "rebind.attacker.test"

	login := h.do(request{
		method: http.MethodPost, path: "/api/v1/admin/login", body: `{"password":"CHANGEME"}`,
		host: rebound, origin: "http://" + rebound,
	})
	if login.Code != http.StatusForbidden {
		t.Fatalf("rebound bootstrap login status = %d, want 403", login.Code)
	}
	if cookies := login.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("rebound login set cookies: %v", cookies)
	}

	bootstrap := h.bootstrapCookie()
	change := h.do(request{
		method: http.MethodPost, path: "/api/v1/admin/password", host: rebound, origin: "http://" + rebound,
		body:    `{"currentPassword":"CHANGEME","newPassword":` + quote(testNewPassword) + `}`,
		cookies: []*http.Cookie{bootstrap},
	})
	if change.Code != http.StatusForbidden {
		t.Errorf("rebound password change status = %d, want 403", change.Code)
	}
	session := h.do(request{method: http.MethodGet, path: "/api/v1/admin/session", host: rebound, cookies: []*http.Cookie{bootstrap}})
	if session.Code != http.StatusForbidden {
		t.Errorf("rebound session read status = %d, want 403", session.Code)
	}
}

func TestImplicitOriginAcceptsLoopbackHosts(t *testing.T) {
	for _, host := range []string{"localhost:8000", "LOCALHOST", "127.0.0.1:8000", "127.1.2.3:9000", "[::1]:8000"} {
		h := newAPI(t, httpapi.SecuritySettings{})
		response := h.do(request{
			method: http.MethodPost, path: "/api/v1/admin/login", body: `{"password":"CHANGEME"}`,
			host: host, origin: "http://" + host,
		})
		if response.Code != http.StatusOK {
			t.Errorf("host %q status = %d, want 200", host, response.Code)
		}
	}
}

func TestImplicitOriginRejectsNonLoopbackHosts(t *testing.T) {
	for _, host := range []string{"192.168.1.10:8000", "0.0.0.0:8000", "localhost.attacker.test", "127.0.0.1.nip.io:8000", "[::ffff:10.0.0.1]:8000"} {
		h := newAPI(t, httpapi.SecuritySettings{})
		response := h.do(request{
			method: http.MethodPost, path: "/api/v1/admin/login", body: `{"password":"CHANGEME"}`,
			host: host, origin: "http://" + host,
		})
		if response.Code != http.StatusForbidden {
			t.Errorf("host %q status = %d, want 403", host, response.Code)
		}
	}
}

func TestPasswordChangeThrottleReturns429(t *testing.T) {
	h := newAPI(t, httpapi.SecuritySettings{})
	session := h.initialize()
	_, state := h.sessionState(session)
	csrf := state["csrfToken"].(string)
	change := func() *httptest.ResponseRecorder {
		return h.do(request{
			method: http.MethodPost, path: "/api/v1/admin/password", origin: testOrigin, csrf: csrf, cookies: []*http.Cookie{session},
			body: `{"currentPassword":"wrong current","newPassword":"another good passphrase"}`,
		})
	}

	for range 2 {
		if response := change(); response.Code != http.StatusForbidden {
			t.Fatalf("wrong current password status = %d, want 403", response.Code)
		}
	}
	response := change()
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.Code)
	}
	if response.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	h.clock.Advance(10 * time.Second)
	if got := change().Header().Get("Retry-After"); got != "50" {
		t.Errorf("Retry-After 10s into the window = %q, want 50", got)
	}
}
