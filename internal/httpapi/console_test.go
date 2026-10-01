package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/httpapi"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

var consoleSettings = []httpapi.Setting{
	{Key: "WEBPTY_ADDRESS", Group: "Server", Label: "Listen address", Value: "127.0.0.1:8000", RestartRequired: true},
	{Key: "WEBPTY_RECORDING_ENABLED", Group: "Recording", Label: "Record new terminals", Value: "true", RestartRequired: true},
}

func newConsoleAPI(t *testing.T) *apiHarness {
	t.Helper()
	return newAPIWith(t, httpapi.SecuritySettings{}, func(service *auth.Service, s *store.Store) []httpapi.Option {
		return []httpapi.Option{httpapi.WithConsole(service, httpapi.SecuritySettings{}, s, consoleSettings)}
	})
}

func TestConsoleRoutesRequireAnAdminSession(t *testing.T) {
	h := newConsoleAPI(t)
	bootstrapCookie := h.bootstrapCookie()
	for _, path := range []string{"/api/v1/admin/audit", "/api/v1/admin/settings"} {
		if response := h.do(request{method: http.MethodGet, path: path}); response.Code != http.StatusUnauthorized {
			t.Errorf("%s anonymous = %d, want 401", path, response.Code)
		}
		bootstrap := h.do(request{method: http.MethodGet, path: path, cookies: []*http.Cookie{bootstrapCookie}})
		if bootstrap.Code != http.StatusForbidden {
			t.Errorf("%s bootstrap = %d, want 403", path, bootstrap.Code)
		}
	}
	session := h.initialize()
	for _, path := range []string{"/api/v1/admin/audit", "/api/v1/admin/settings"} {
		foreign := h.do(request{method: http.MethodGet, path: path, host: "evil.example", cookies: []*http.Cookie{session}})
		if foreign.Code != http.StatusForbidden {
			t.Errorf("%s foreign host = %d, want 403", path, foreign.Code)
		}
	}
}

func TestAuditRouteIsPaginatedNewestFirst(t *testing.T) {
	h := newConsoleAPI(t)
	session := h.initialize()
	for i := range 3 {
		if err := h.store.AppendAuditEvent(context.Background(), store.AuditEvent{
			OccurredAt: time.Unix(1_800_000_000+int64(i), 0), Type: fmt.Sprintf("test.%d", i),
			RemoteAddr: "198.51.100.7", Details: map[string]string{"terminalId": "t1"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	first := h.do(request{method: http.MethodGet, path: "/api/v1/admin/audit?limit=2", cookies: []*http.Cookie{session}})
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d %s", first.Code, first.Body)
	}
	if first.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", first.Header().Get("Cache-Control"))
	}
	body := decodeJSON(t, first)
	events := body["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("events = %v", events)
	}
	newest := events[0].(map[string]any)
	if newest["type"] != "test.2" || newest["remoteAddr"] != "198.51.100.7" ||
		newest["occurredAt"] != "2027-01-15T08:00:02Z" || newest["details"].(map[string]any)["terminalId"] != "t1" {
		t.Fatalf("newest = %v", newest)
	}
	if _, ok := newest["id"].(string); !ok {
		t.Fatalf("id = %v, want string", newest["id"])
	}
	cursor, ok := body["nextCursor"].(string)
	if !ok || cursor == "" {
		t.Fatalf("nextCursor = %v", body["nextCursor"])
	}

	rest := decodeJSON(t, h.do(request{method: http.MethodGet, path: "/api/v1/admin/audit?limit=2&cursor=" + cursor, cookies: []*http.Cookie{session}}))
	older := rest["events"].([]any)
	if len(older) != 2 || older[0].(map[string]any)["type"] != "test.0" {
		t.Fatalf("second page = %v", older)
	}

	all := decodeJSON(t, h.do(request{method: http.MethodGet, path: "/api/v1/admin/audit?limit=200", cookies: []*http.Cookie{session}}))
	if all["nextCursor"] != nil {
		t.Fatalf("complete page nextCursor = %v, want null", all["nextCursor"])
	}
}

func TestAuditRouteValidatesQuery(t *testing.T) {
	h := newConsoleAPI(t)
	session := h.initialize()
	for _, query := range []string{"limit=0", "limit=201", "limit=x", "cursor=-1", "cursor=abc"} {
		response := h.do(request{method: http.MethodGet, path: "/api/v1/admin/audit?" + query, cookies: []*http.Cookie{session}})
		if response.Code != http.StatusBadRequest || decodeJSON(t, response)["code"] != "invalid_argument" {
			t.Errorf("%s = %d", query, response.Code)
		}
	}
}

func TestSettingsRouteReturnsOnlyConfiguredValues(t *testing.T) {
	h := newConsoleAPI(t)
	response := h.do(request{method: http.MethodGet, path: "/api/v1/admin/settings", cookies: []*http.Cookie{h.initialize()}})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	body := decodeJSON(t, response)
	if body["readOnly"] != true {
		t.Errorf("readOnly = %v", body["readOnly"])
	}
	settings := body["settings"].([]any)
	if len(settings) != 2 {
		t.Fatalf("settings = %v", settings)
	}
	first := settings[0].(map[string]any)
	if first["key"] != "WEBPTY_ADDRESS" || first["group"] != "Server" || first["label"] != "Listen address" ||
		first["value"] != "127.0.0.1:8000" || first["restartRequired"] != true {
		t.Fatalf("first = %v", first)
	}
	if strings.Contains(response.Body.String(), "CHANGEME") {
		t.Fatal("settings leaked a password")
	}
}
