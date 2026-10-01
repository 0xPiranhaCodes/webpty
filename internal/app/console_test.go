package app_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestServeExposesSanitizedSettings(t *testing.T) {
	cfg := testConfig(t)
	cfg.Command = "/bin/sh"
	cfg.CommandArgs = []string{"-c", "deploy --token=s3cret-value"}
	c, _, _ := serveAdmin(t, cfg)

	response := c.do(http.MethodGet, "/api/v1/admin/settings", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("settings = %d", response.StatusCode)
	}
	raw, _ := io.ReadAll(response.Body)
	body := string(raw)
	for _, secret := range []string{"s3cret-value", "deploy", "CHANGEME", "a much better passphrase"} {
		if strings.Contains(body, secret) {
			t.Errorf("settings leaked %q: %s", secret, body)
		}
	}
	for _, want := range []string{`"WEBPTY_COMMAND"`, `"/bin/sh"`, `"WEBPTY_RECORDING_RETENTION"`, `"720h0m0s"`, `"2 arguments"`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings lack %s: %s", want, body)
		}
	}
}

func TestServeMountsAuditAndWebUI(t *testing.T) {
	c, _, _ := serveAdmin(t, testConfig(t))

	audit := decode(t, c.do(http.MethodGet, "/api/v1/admin/audit?limit=5", ""))
	events, _ := audit["events"].([]any)
	if len(events) == 0 {
		t.Fatalf("audit = %v, want the bootstrap events", audit)
	}

	page := c.do(http.MethodGet, "/admin/sessions", "")
	if page.StatusCode != http.StatusOK && page.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/admin/sessions = %d", page.StatusCode)
	}
	if page.Header.Get("Content-Security-Policy") == "" || page.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("web UI lacks security headers: %v", page.Header)
	}
	if health := c.do(http.MethodGet, "/healthz", ""); health.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", health.StatusCode)
	}
}
