package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0xPiranhaCodes/webpty/internal/access"
	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

const (
	sessionCookie   = "webpty_session"
	bootstrapCookie = "webpty_bootstrap"
	csrfHeader      = "X-CSRF-Token"
	maxBodyBytes    = 8 << 10
)

// SecuritySettings control cookie and origin policy.
type SecuritySettings struct {
	// PublicOrigin, when set, is the only origin accepted for mutating
	// requests. Otherwise requests must target a loopback Host and mutating
	// requests must originate from that same Host, which defeats DNS
	// rebinding against local installations.
	PublicOrigin  string
	SecureCookies bool
}

type adminAPI struct {
	service  *auth.Service
	security SecuritySettings
	// guests, when set, lets admin logout also revoke an access session
	// the same browser holds.
	guests *access.Service
}

// hostCookiePrefix binds cookies to the exact origin in secure mode: the
// browser only accepts them over HTTPS, with Path=/ and no Domain, so a
// network attacker or sibling subdomain cannot plant or shadow them.
const hostCookiePrefix = "__Host-"

// cookieName is base, prefixed with __Host- when cookies are secure. Plain
// HTTP development keeps the unprefixed name, which browsers would
// otherwise reject.
func (a *adminAPI) cookieName(base string) string {
	if a.security.SecureCookies {
		return hostCookiePrefix + base
	}
	return base
}

func (a *adminAPI) cookieValue(r *http.Request, base string) string {
	cookie, err := r.Cookie(a.cookieName(base))
	if err != nil {
		return ""
	}
	return cookie.Value
}

type sessionKey struct{}

type currentSession struct {
	auth.Session
	token string
}

// WithAdminAuth mounts the administrator authentication API. When guests
// is given, signing out also revokes the browser's access session.
func WithAdminAuth(service *auth.Service, security SecuritySettings, guests ...*access.Service) Option {
	api := &adminAPI{service: service, security: security}
	if len(guests) > 0 {
		api.guests = guests[0]
	}
	return func(mux *http.ServeMux) {
		mux.Handle("POST /api/v1/admin/login", api.trustedHost(api.sameOrigin(http.HandlerFunc(api.login))))
		mux.Handle("POST /api/v1/admin/password", api.trustedHost(api.sameOrigin(api.authenticated(api.csrf(http.HandlerFunc(api.changePassword))))))
		mux.Handle("POST /api/v1/admin/logout", api.trustedHost(api.sameOrigin(api.authenticated(api.csrf(http.HandlerFunc(api.logout))))))
		mux.Handle("GET /api/v1/admin/session", api.trustedHost(api.authenticated(http.HandlerFunc(api.session))))
	}
}

func (a *adminAPI) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	session, err := a.service.Login(r.Context(), body.Password, remoteIP(r))
	switch {
	case errors.Is(err, auth.ErrThrottled):
		setRetryAfter(w, throttleWait(err))
		writeError(w, http.StatusTooManyRequests, "too many attempts")
		return
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	case err != nil:
		internalError(w, "login", err)
		return
	}
	a.setSessionCookie(w, session)
	writeJSON(w, http.StatusOK, struct {
		PasswordChangeRequired bool `json:"passwordChangeRequired"`
	}{session.PasswordChangeRequired()})
}

func throttleWait(err error) time.Duration {
	var throttled *auth.ThrottledError
	if errors.As(err, &throttled) {
		return throttled.RetryAfter
	}
	return 0
}

// setRetryAfter tells a throttled client how many whole seconds to wait,
// rounded up so a retry at that time is accepted, and never less than 1.
func setRetryAfter(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
}

func (a *adminAPI) changePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	current := sessionFrom(r.Context())
	session, err := a.service.ChangePassword(r.Context(), current.token, body.CurrentPassword, body.NewPassword, remoteIP(r))
	switch {
	case errors.Is(err, auth.ErrThrottled):
		setRetryAfter(w, throttleWait(err))
		writeError(w, http.StatusTooManyRequests, "too many attempts")
		return
	case errors.Is(err, auth.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "unauthenticated")
		return
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusForbidden, "invalid current password")
		return
	case errors.Is(err, auth.ErrInvalidNewPassword):
		writeError(w, http.StatusBadRequest, "new password must be at least 12 characters and must not be the default password")
		return
	case err != nil:
		internalError(w, "change password", err)
		return
	}
	a.setSessionCookie(w, session)
	w.WriteHeader(http.StatusNoContent)
}

// logout revokes every session the browser presents: the authenticated
// one, any other admin or bootstrap cookie, and its access session.
func (a *adminAPI) logout(w http.ResponseWriter, r *http.Request) {
	current := sessionFrom(r.Context())
	remote := remoteIP(r)
	if err := a.service.Logout(r.Context(), current.token, remote); err != nil {
		internalError(w, "logout", err)
		return
	}
	for _, base := range []string{sessionCookie, bootstrapCookie} {
		if token := a.cookieValue(r, base); token != "" && token != current.token {
			if err := a.service.Logout(r.Context(), token, remote); err != nil {
				internalError(w, "logout", err)
				return
			}
		}
	}
	if token := a.cookieValue(r, accessCookie); token != "" && a.guests != nil {
		if err := a.guests.Logout(r.Context(), token, remote); err != nil && !errors.Is(err, access.ErrUnauthenticated) {
			internalError(w, "logout", err)
			return
		}
	}
	a.clearCookie(w, sessionCookie)
	a.clearCookie(w, bootstrapCookie)
	a.clearCookie(w, accessCookie)
	w.WriteHeader(http.StatusNoContent)
}

func (a *adminAPI) session(w http.ResponseWriter, r *http.Request) {
	current := sessionFrom(r.Context())
	state := "authenticated"
	if current.PasswordChangeRequired() {
		state = "bootstrap"
	}
	writeJSON(w, http.StatusOK, struct {
		State                  string    `json:"state"`
		PasswordChangeRequired bool      `json:"passwordChangeRequired"`
		CSRFToken              string    `json:"csrfToken"`
		ExpiresAt              time.Time `json:"expiresAt"`
	}{state, current.PasswordChangeRequired(), auth.CSRFToken(current.token), current.ExpiresAt.UTC()})
}

// authenticated resolves the session cookie. An admin session is only
// accepted from the session cookie and a bootstrap session only from the
// bootstrap cookie.
func (a *adminAPI) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current, err := a.resolve(r)
		if errors.Is(err, auth.ErrUnauthenticated) {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		if err != nil {
			internalError(w, "authenticate", err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, current)))
	})
}

func (a *adminAPI) resolve(r *http.Request) (currentSession, error) {
	for _, candidate := range []struct {
		cookie string
		kind   store.SessionKind
	}{{sessionCookie, store.SessionKindAdmin}, {bootstrapCookie, store.SessionKindBootstrap}} {
		token := a.cookieValue(r, candidate.cookie)
		if token == "" {
			continue
		}
		session, err := a.service.Authenticate(r.Context(), token)
		if errors.Is(err, auth.ErrUnauthenticated) {
			continue
		}
		if err != nil {
			return currentSession{}, err
		}
		if session.Kind != candidate.kind || !session.ExpiresAt.After(a.service.Now()) {
			continue
		}
		return currentSession{Session: session, token: token}, nil
	}
	return currentSession{}, auth.ErrUnauthenticated
}

// adminRoutes returns the middleware chains for administrator reads and
// mutations. Mutations also require the same origin and a CSRF token.
func (a *adminAPI) adminRoutes() (read, mutate func(http.HandlerFunc) http.Handler) {
	read = func(h http.HandlerFunc) http.Handler { return a.trustedHost(a.authenticated(adminOnly(h))) }
	mutate = func(h http.HandlerFunc) http.Handler {
		return a.trustedHost(a.sameOrigin(a.authenticated(adminOnly(a.csrf(h)))))
	}
	return read, mutate
}

// adminOnly rejects bootstrap sessions, which may only rotate the password.
func adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sessionFrom(r.Context()).Kind != store.SessionKindAdmin {
			writeError(w, http.StatusForbidden, "password change required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrf requires the session-bound CSRF token for admin sessions. Bootstrap
// sessions rely on SameSite=Strict, the bootstrap cookie, and origin checks.
func (a *adminAPI) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := sessionFrom(r.Context())
		if current.Kind != store.SessionKindBootstrap && !auth.VerifyCSRFToken(current.token, r.Header.Get(csrfHeader)) {
			writeError(w, http.StatusForbidden, "invalid CSRF token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// trustedHost rejects non-loopback Host headers unless a public origin is
// configured.
func (a *adminAPI) trustedHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.security.PublicOrigin == "" && !isLoopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, "untrusted host; configure WEBPTY_PUBLIC_ORIGIN")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sameOrigin rejects requests whose Origin (or, absent that, Referer) does
// not match the expected origin. Requests carrying neither are rejected.
func (a *adminAPI) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.originAllowed(r) {
			writeError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// expectedOrigin is the configured public origin, or the request's own
// origin when none is configured.
func (a *adminAPI) expectedOrigin(r *http.Request) string {
	if a.security.PublicOrigin != "" {
		return a.security.PublicOrigin
	}
	scheme := "http"
	if r.TLS != nil || a.security.SecureCookies {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (a *adminAPI) originAllowed(r *http.Request) bool {
	expected := a.expectedOrigin(r)
	origin := r.Header.Get("Origin")
	if origin == "" {
		referer, err := url.Parse(r.Header.Get("Referer"))
		if err != nil || referer.Scheme == "" || referer.Host == "" {
			return false
		}
		origin = referer.Scheme + "://" + referer.Host
	}
	return strings.EqualFold(origin, expected)
}

func (a *adminAPI) setSessionCookie(w http.ResponseWriter, session auth.Session) {
	name, other := sessionCookie, bootstrapCookie
	if session.PasswordChangeRequired() {
		name, other = bootstrapCookie, sessionCookie
	}
	maxAge := int(session.ExpiresAt.Sub(a.service.Now()).Seconds())
	http.SetCookie(w, a.cookie(name, session.Token, maxAge))
	a.clearCookie(w, other)
}

func (a *adminAPI) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, a.cookie(name, "", -1))
}

// cookie builds the cookie named base (see cookieName).
func (a *adminAPI) cookie(base, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     a.cookieName(base),
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   a.security.SecureCookies,
		SameSite: http.SameSiteStrictMode,
	}
}

func sessionFrom(ctx context.Context) currentSession {
	current, _ := ctx.Value(sessionKey{}).(currentSession)
	return current
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	if decoder.More() {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{message})
}

func internalError(w http.ResponseWriter, operation string, err error) {
	log.Printf("admin %s: %v", operation, err)
	writeError(w, http.StatusInternalServerError, "internal error")
}
