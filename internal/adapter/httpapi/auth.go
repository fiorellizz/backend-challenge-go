package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/fiorellizz/backend-challenge-go/internal/platform/auth"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
)

// Authorization model. Two identities exist, told apart by realm role:
//
//	provider          submits operations and reads its own transactions. The
//	                  token carries the provider id in a claim; the request's
//	                  providerId (body or path) must match it.
//	internal-service  opens and reads wallets, runs reconciliation and reads
//	                  any transaction by internal id.
//
// Health and metrics are public. Everything else requires a valid bearer
// token; a valid token without the right role gets 403.

// Auth builds the guards handlers wrap their routes with.
type Auth struct {
	verifier auth.Verifier
	roles    config.OIDC
	log      *slog.Logger
}

// NewAuth builds the guards around the token verifier.
func NewAuth(verifier auth.Verifier, cfg config.Config, log *slog.Logger) *Auth {
	return &Auth{verifier: verifier, roles: cfg.OIDC, log: log.With("component", "auth")}
}

// ErrForbidden marks a valid identity lacking permission for the resource.
var ErrForbidden = errors.New("forbidden")

// Internal allows only the internal service role.
func (a *Auth) Internal(next http.HandlerFunc) http.HandlerFunc {
	return a.authenticated(func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		if !p.HasRole(a.roles.InternalRole) {
			writeError(w, r, a.log, fmt.Errorf("%w: internal role required", ErrForbidden))
			return
		}
		next(w, r)
	})
}

// Provider allows only provider tokens that carry a provider id. Handlers
// then compare that id with the request's providerId via SameProvider.
func (a *Auth) Provider(next http.HandlerFunc) http.HandlerFunc {
	return a.authenticated(func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		if !p.HasRole(a.roles.ProviderRole) || p.ProviderID == "" {
			writeError(w, r, a.log, fmt.Errorf("%w: provider role required", ErrForbidden))
			return
		}
		next(w, r)
	})
}

// Any allows either role; the handler decides what the caller may see.
func (a *Auth) Any(next http.HandlerFunc) http.HandlerFunc {
	return a.authenticated(func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		if !p.HasRole(a.roles.InternalRole) && !(p.HasRole(a.roles.ProviderRole) && p.ProviderID != "") {
			writeError(w, r, a.log, fmt.Errorf("%w: no permitted role", ErrForbidden))
			return
		}
		next(w, r)
	})
}

// IsInternal reports whether the principal has the internal role.
func (a *Auth) IsInternal(p auth.Principal) bool { return p.HasRole(a.roles.InternalRole) }

func (a *Auth) authenticated(next func(http.ResponseWriter, *http.Request, auth.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wager"`)
			writeError(w, r, a.log, fmt.Errorf("%w: missing bearer token", auth.ErrUnauthenticated))
			return
		}
		p, err := a.verifier.Verify(r.Context(), raw)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wager", error="invalid_token"`)
			// The reason is logged, never returned: clients learn only that
			// the credential was refused.
			a.log.InfoContext(r.Context(), "token refused", "path", r.URL.Path, "reason", err.Error())
			writeError(w, r, a.log, fmt.Errorf("%w: invalid or expired token", auth.ErrUnauthenticated))
			return
		}
		next(w, r.WithContext(auth.WithPrincipal(r.Context(), p)), p)
	}
}

// SameProvider enforces the isolation rule: a provider may only act on its
// own providerId. Returns the error to write when it does not match.
func SameProvider(r *http.Request, providerID string) error {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.ProviderID != providerID {
		return fmt.Errorf("%w: token is not bound to provider %q", ErrForbidden, providerID)
	}
	return nil
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}
