package httpapi

import (
	"context"
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/access"
	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/collab"
	"github.com/0xPiranhaCodes/webpty/internal/session"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

const accessCookie = "webpty_access"

// invitePath is where the SPA accepts invitations. The token travels in the
// fragment, so it is never sent to the server or in a Referer.
const invitePath = "/join#token="

// viewer is whoever opened a terminal connection: the administrator (owner)
// or a guest holding an access session.
type viewer struct {
	owner bool
	token string
	guest access.Session
}

type viewerKey struct{}

func viewerFrom(ctx context.Context) viewer {
	v, _ := ctx.Value(viewerKey{}).(viewer)
	return v
}

func (v viewer) role() collab.Role {
	if v.owner {
		return collab.RoleOwner
	}
	if v.guest.Grant.Role == store.AccessEditor {
		return collab.RoleEditor
	}
	return collab.RoleViewer
}

type permissionsJSON struct {
	Input  bool `json:"input"`
	Resize bool `json:"resize"`
}

func permissionsFor(role collab.Role) permissionsJSON {
	return permissionsJSON{Input: role.Allows(collab.ActionInput), Resize: role.Allows(collab.ActionResize)}
}

// sharedTerminalJSON is the terminal metadata guests may see. The command,
// its arguments, and failure details are admin-only.
type sharedTerminalJSON struct {
	ID         string     `json:"id"`
	State      string     `json:"state"`
	Rows       int        `json:"rows"`
	Cols       int        `json:"cols"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt"`
	EndedAt    *time.Time `json:"endedAt"`
	ExitCode   *int       `json:"exitCode"`
	ExitSignal string     `json:"exitSignal,omitempty"`
}

func toSharedTerminalJSON(info session.Info) sharedTerminalJSON {
	full := toTerminalJSON(info)
	return sharedTerminalJSON{
		ID: full.ID, State: full.State, Rows: full.Rows, Cols: full.Cols, CreatedAt: full.CreatedAt,
		StartedAt: full.StartedAt, EndedAt: full.EndedAt, ExitCode: full.ExitCode, ExitSignal: full.ExitSignal,
	}
}

type grantJSON struct {
	ID              string     `json:"id"`
	TerminalID      string     `json:"terminalId"`
	Role            string     `json:"role"`
	Label           string     `json:"label"`
	SingleUse       bool       `json:"singleUse"`
	MaxRedemptions  int        `json:"maxRedemptions"`
	RedemptionCount int        `json:"redemptionCount"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	ExpiresAt       time.Time  `json:"expiresAt"`
	RedeemedAt      *time.Time `json:"redeemedAt"`
	RevokedAt       *time.Time `json:"revokedAt"`
	RevokeReason    string     `json:"revokeReason,omitempty"`
}

func (api *terminalAPI) toGrantJSON(grant store.AccessGrant) grantJSON {
	optional := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		utc := t.UTC()
		return &utc
	}
	return grantJSON{
		ID: grant.PublicID, TerminalID: grant.TerminalID, Role: string(grant.Role), Label: grant.Label,
		SingleUse: grant.SingleUse, MaxRedemptions: grant.MaxRedemptions, RedemptionCount: grant.RedemptionCount,
		Status: access.Status(grant, api.access.Now()), CreatedAt: grant.CreatedAt.UTC(), UpdatedAt: grant.UpdatedAt.UTC(),
		ExpiresAt: grant.ExpiresAt.UTC(), RedeemedAt: optional(grant.RedeemedAt), RevokedAt: optional(grant.RevokedAt),
		RevokeReason: grant.RevokeReason,
	}
}

func (api *terminalAPI) mountAccess(mux *http.ServeMux, read, mutate func(http.HandlerFunc) http.Handler) {
	a := api.admin
	mux.Handle("POST /api/v1/admin/terminals/{id}/grants", mutate(api.createGrant))
	mux.Handle("GET /api/v1/admin/terminals/{id}/grants", read(api.listGrants))
	mux.Handle("DELETE /api/v1/admin/terminals/{id}/grants/{grantID}", mutate(api.revokeGrant))
	mux.Handle("POST /api/v1/access/redeem", noReferrer(a.trustedHost(a.sameOrigin(http.HandlerFunc(api.redeem)))))
	mux.Handle("GET /api/v1/access/session", a.trustedHost(api.guestAuthenticated(http.HandlerFunc(api.guestSession))))
	mux.Handle("POST /api/v1/access/logout", a.trustedHost(a.sameOrigin(api.guestAuthenticated(api.guestCSRF(http.HandlerFunc(api.guestLogout))))))
}

// noReferrer keeps invitation redemption out of caches and Referer headers.
func noReferrer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func (api *terminalAPI) createGrant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role           string `json:"role"`
		Label          string `json:"label"`
		TTLSeconds     int64  `json:"ttlSeconds"`
		SingleUse      bool   `json:"singleUse"`
		MaxRedemptions int    `json:"maxRedemptions"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.TTLSeconds < 0 || body.TTLSeconds > math.MaxInt64/int64(time.Second) {
		api.writeAccessError(w, "create grant", access.ErrInvalidArgument)
		return
	}
	id := r.PathValue("id")
	info, err := api.manager.Get(r.Context(), id)
	if err != nil {
		api.writeError(w, "create grant", err)
		return
	}
	if info.State != store.TerminalRunning {
		api.writeError(w, "create grant", session.ErrInvalidState)
		return
	}
	invitation, err := api.access.CreateGrant(r.Context(), access.GrantRequest{
		TerminalID: id, Role: store.AccessRole(body.Role), Label: body.Label,
		TTL: time.Duration(body.TTLSeconds) * time.Second, SingleUse: body.SingleUse,
		MaxRedemptions: body.MaxRedemptions, RemoteAddr: remoteIP(r),
	})
	if err != nil {
		api.writeAccessError(w, "create grant", err)
		return
	}
	replaced := make([]string, 0, len(invitation.Replaced))
	for _, old := range invitation.Replaced {
		replaced = append(replaced, old.PublicID)
	}
	writeJSON(w, http.StatusCreated, struct {
		Grant     grantJSON `json:"grant"`
		Token     string    `json:"token"`
		InviteURL string    `json:"inviteUrl"`
		Replaced  []string  `json:"replaced"`
	}{api.toGrantJSON(invitation.Grant), invitation.Token, api.admin.expectedOrigin(r) + invitePath + invitation.Token, replaced})
}

func (api *terminalAPI) listGrants(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := api.manager.Get(r.Context(), id); err != nil {
		api.writeError(w, "list grants", err)
		return
	}
	grants, err := api.access.Grants(r.Context(), id)
	if err != nil {
		api.writeAccessError(w, "list grants", err)
		return
	}
	body := struct {
		Grants []grantJSON `json:"grants"`
	}{Grants: make([]grantJSON, 0, len(grants))}
	for _, grant := range grants {
		body.Grants = append(body.Grants, api.toGrantJSON(grant))
	}
	writeJSON(w, http.StatusOK, body)
}

func (api *terminalAPI) revokeGrant(w http.ResponseWriter, r *http.Request) {
	grant, err := api.access.RevokeGrant(r.Context(), r.PathValue("id"), r.PathValue("grantID"), remoteIP(r))
	if err != nil {
		api.writeAccessError(w, "revoke grant", err)
		return
	}
	writeJSON(w, http.StatusOK, api.toGrantJSON(grant))
}

type guestSessionJSON struct {
	Role        string             `json:"role"`
	GrantID     string             `json:"grantId"`
	Terminal    sharedTerminalJSON `json:"terminal"`
	Permissions permissionsJSON    `json:"permissions"`
	ExpiresAt   time.Time          `json:"expiresAt"`
	CSRFToken   string             `json:"csrfToken"`
}

func guestSessionBody(current access.Session, token string, info session.Info) guestSessionJSON {
	role := viewer{guest: current}.role()
	return guestSessionJSON{
		Role: string(current.Grant.Role), GrantID: current.Grant.PublicID, Terminal: toSharedTerminalJSON(info),
		Permissions: permissionsFor(role), ExpiresAt: current.ExpiresAt.UTC(), CSRFToken: access.CSRFToken(token),
	}
}

func (api *terminalAPI) redeem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	// Tokens carry 256 random bits, so throttling only bounds the cost of
	// guessing; it counts every attempt, successful or not.
	if decision := api.settings.RedeemThrottle.Check(auth.ThrottleKey(remoteIP(r))); !decision.Allowed {
		if decision.FirstDenial {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			if err := api.access.Audit(ctx, "access.redeem.throttled", remoteIP(r), nil); err != nil {
				api.settings.Logger.Error("append audit event", "type", "access.redeem.throttled", "error", err)
			}
			cancel()
		}
		setRetryAfter(w, decision.RetryAfter)
		writeJSON(w, http.StatusTooManyRequests, struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}{"too many attempts", "rate_limited"})
		return
	}
	// The terminal metadata is read inside the redemption so that a failed
	// lookup leaves the invitation unused, and nothing after it can fail.
	var info session.Info
	var lookupErr error
	// The browser's previous access session ends with the redemption, so one
	// browser never holds two guest identities.
	previous := api.admin.cookieValue(r, accessCookie)
	current, err := api.access.RedeemReplacing(r.Context(), body.Token, previous, remoteIP(r), func(grant store.AccessGrant) error {
		info, lookupErr = api.manager.Get(r.Context(), grant.TerminalID)
		return lookupErr
	})
	switch {
	case lookupErr != nil:
		api.writeError(w, "redeem", lookupErr)
		return
	case err != nil:
		api.writeAccessError(w, "redeem", err)
		return
	}
	maxAge := int(current.ExpiresAt.Sub(api.access.Now()).Seconds())
	http.SetCookie(w, api.admin.cookie(accessCookie, current.Token, max(maxAge, 1)))
	writeJSON(w, http.StatusOK, guestSessionBody(current, current.Token, info))
}

func (api *terminalAPI) guestSession(w http.ResponseWriter, r *http.Request) {
	current := viewerFrom(r.Context())
	info, err := api.manager.Get(r.Context(), current.guest.Grant.TerminalID)
	if err != nil {
		api.writeError(w, "access session", err)
		return
	}
	writeJSON(w, http.StatusOK, guestSessionBody(current.guest, current.token, info))
}

func (api *terminalAPI) guestLogout(w http.ResponseWriter, r *http.Request) {
	current := viewerFrom(r.Context())
	err := api.access.Logout(r.Context(), current.token, remoteIP(r))
	if err != nil && !errors.Is(err, access.ErrUnauthenticated) {
		api.writeAccessError(w, "access logout", err)
		return
	}
	api.admin.clearCookie(w, accessCookie)
	w.WriteHeader(http.StatusNoContent)
}

// guestAuthenticated resolves the access-session cookie.
func (api *terminalAPI) guestAuthenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current, token, err := api.resolveGuest(r)
		if errors.Is(err, access.ErrUnauthenticated) {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		if err != nil {
			internalError(w, "authenticate access session", err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), viewerKey{}, viewer{token: token, guest: current})))
	})
}

func (api *terminalAPI) resolveGuest(r *http.Request) (access.Session, string, error) {
	token := api.admin.cookieValue(r, accessCookie)
	if token == "" {
		return access.Session{}, "", access.ErrUnauthenticated
	}
	current, err := api.access.Authenticate(r.Context(), token)
	if err != nil {
		return access.Session{}, "", err
	}
	return current, token, nil
}

// guestCSRF requires the CSRF token bound to the access session.
func (api *terminalAPI) guestCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !access.VerifyCSRFToken(viewerFrom(r.Context()).token, r.Header.Get(csrfHeader)) {
			writeError(w, http.StatusForbidden, "invalid CSRF token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// terminalViewer admits the administrator, or a guest whose access session
// is for the requested terminal. A bootstrap session is not an owner, but it
// does not hide a valid access session from the same browser either.
func (api *terminalAPI) terminalViewer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current, err := api.admin.resolve(r)
		bootstrap := err == nil && current.Kind != store.SessionKindAdmin
		switch {
		case bootstrap && (api.access == nil || api.admin.cookieValue(r, accessCookie) == ""):
			writeError(w, http.StatusForbidden, "password change required")
			return
		case bootstrap:
		case err == nil:
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), viewerKey{}, viewer{owner: true, token: current.token})))
			return
		case !errors.Is(err, auth.ErrUnauthenticated):
			internalError(w, "authenticate", err)
			return
		}
		if api.access == nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		guest, token, err := api.resolveGuest(r)
		switch {
		case errors.Is(err, access.ErrUnauthenticated) && bootstrap:
			writeError(w, http.StatusForbidden, "password change required")
		case errors.Is(err, access.ErrUnauthenticated):
			writeError(w, http.StatusUnauthorized, "unauthenticated")
		case err != nil:
			internalError(w, "authenticate access session", err)
		case guest.Grant.TerminalID != r.PathValue("id"):
			writeError(w, http.StatusForbidden, "access is not valid for this terminal")
		default:
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), viewerKey{}, viewer{token: token, guest: guest})))
		}
	})
}

// writeAccessError maps access errors to stable status codes and error codes.
func (api *terminalAPI) writeAccessError(w http.ResponseWriter, operation string, err error) {
	status, code, message := http.StatusInternalServerError, "internal", "internal error"
	switch {
	case errors.Is(err, access.ErrInvalidToken):
		status, code, message = http.StatusUnauthorized, "invalid_invitation", "invalid or expired invitation"
	case errors.Is(err, access.ErrUnauthenticated):
		status, code, message = http.StatusUnauthorized, "unauthenticated", "unauthenticated"
	case errors.Is(err, access.ErrNotFound):
		status, code, message = http.StatusNotFound, "not_found", "not found"
	case errors.Is(err, access.ErrInvalidArgument):
		status, code, message = http.StatusBadRequest, "invalid_argument", "invalid grant request"
	default:
		api.settings.Logger.Error("access api", "operation", operation, "error", err)
	}
	writeJSON(w, status, struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}{message, code})
}
