// Package auth defines the authenticated identity the HTTP layer works
// with and the port that produces it from a bearer token. The OIDC adapter
// implements the port; handlers only see Principal.
package auth

import (
	"context"
	"errors"
)

// ErrUnauthenticated marks a missing, malformed, expired or otherwise
// unverifiable credential.
var ErrUnauthenticated = errors.New("unauthenticated")

// Principal is the identity carried by a verified token.
type Principal struct {
	// Subject is the token's sub claim (the service account user).
	Subject string
	// ClientID is the OAuth client that obtained the token (azp).
	ClientID string
	// Roles are the realm roles granted to the client.
	Roles []string
	// ProviderID is the provider the token is bound to; empty for
	// identities that are not providers.
	ProviderID string
}

// HasRole reports whether the principal carries the role.
func (p Principal) HasRole(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// Verifier validates a raw bearer token and extracts its principal.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (Principal, error)
}

type ctxKey struct{}

// WithPrincipal stores the principal in the context for handlers.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal stored by the authentication
// middleware; ok is false on unauthenticated requests.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
