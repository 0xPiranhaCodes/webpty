package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

const defaultAuditPage = 50

// Setting is one sanitized runtime configuration value shown to the
// administrator. Values must never include credentials, tokens, or command
// arguments.
type Setting struct {
	Key             string `json:"key"`
	Group           string `json:"group"`
	Label           string `json:"label"`
	Value           string `json:"value"`
	RestartRequired bool   `json:"restartRequired"`
}

type consoleAPI struct {
	store    *store.Store
	settings []Setting
}

// WithConsole mounts the admin audit log and the read-only runtime settings.
func WithConsole(service *auth.Service, security SecuritySettings, db *store.Store, settings []Setting) Option {
	a := &adminAPI{service: service, security: security}
	api := &consoleAPI{store: db, settings: append([]Setting(nil), settings...)}
	read, _ := a.adminRoutes()
	return func(mux *http.ServeMux) {
		mux.Handle("GET /api/v1/admin/audit", read(api.audit))
		mux.Handle("GET /api/v1/admin/settings", read(api.runtimeSettings))
	}
}

type auditEventJSON struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	OccurredAt time.Time         `json:"occurredAt"`
	RemoteAddr string            `json:"remoteAddr"`
	Details    map[string]string `json:"details"`
}

func (api *consoleAPI) audit(w http.ResponseWriter, r *http.Request) {
	cursor, ok := queryInt(w, r, "cursor")
	if !ok {
		return
	}
	limit, ok := queryInt(w, r, "limit")
	if !ok {
		return
	}
	if r.URL.Query().Get("limit") == "" {
		limit = defaultAuditPage
	}
	if limit < 1 || limit > store.MaxAuditPage {
		writeCodedError(w, http.StatusBadRequest, "invalid_argument", "limit must be 1.."+strconv.Itoa(store.MaxAuditPage))
		return
	}
	page, err := api.store.AuditPage(r.Context(), cursor, int(limit))
	if err != nil {
		internalError(w, "audit page", err)
		return
	}
	body := struct {
		Events     []auditEventJSON `json:"events"`
		NextCursor *string          `json:"nextCursor"`
	}{Events: make([]auditEventJSON, 0, len(page.Events))}
	for _, event := range page.Events {
		details := event.Details
		if details == nil {
			details = map[string]string{}
		}
		body.Events = append(body.Events, auditEventJSON{
			ID: strconv.FormatInt(event.ID, 10), Type: event.Type, OccurredAt: event.OccurredAt.UTC(),
			RemoteAddr: event.RemoteAddr, Details: details,
		})
	}
	if page.Next != 0 {
		next := strconv.FormatInt(page.Next, 10)
		body.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, body)
}

func (api *consoleAPI) runtimeSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		ReadOnly bool      `json:"readOnly"`
		Settings []Setting `json:"settings"`
	}{true, api.settings})
}
