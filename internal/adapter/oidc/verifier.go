// Package oidc verifies bearer tokens issued by an external OpenID Connect
// provider (Keycloak in the local stack). Signing keys come from the
// provider's JWKS endpoint, discovered at start; the service never issues
// tokens nor stores credentials.
package oidc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/fiorellizz/backend-challenge-go/internal/platform/auth"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
)

// Verifier implements auth.Verifier on top of go-oidc.
type Verifier struct {
	cfg      config.OIDC
	log      *slog.Logger
	verifier atomic.Pointer[gooidc.IDTokenVerifier]
}

// NewVerifier builds the verifier. It does no network I/O; call Init from
// the lifecycle start so a slow IdP delays the boot instead of failing it.
func NewVerifier(cfg config.Config, log *slog.Logger) *Verifier {
	return &Verifier{cfg: cfg.OIDC, log: log.With("component", "oidc")}
}

// Init runs OIDC discovery, retrying until it succeeds or ctx expires.
// Discovery is fetched from DiscoveryURL (the in-network address) while
// tokens are checked against IssuerURL (what the IdP writes in `iss`).
func (v *Verifier) Init(ctx context.Context) error {
	discoveryCtx := gooidc.InsecureIssuerURLContext(ctx, v.cfg.IssuerURL)
	for attempt := 1; ; attempt++ {
		provider, err := gooidc.NewProvider(discoveryCtx, v.cfg.DiscoveryURL)
		if err == nil {
			v.verifier.Store(provider.VerifierContext(context.Background(), &gooidc.Config{ClientID: v.cfg.Audience}))
			v.log.Info("oidc discovery completed", "issuer", v.cfg.IssuerURL, "attempt", attempt)
			return nil
		}
		v.log.Warn("oidc discovery failed; retrying", "attempt", attempt, "error", err.Error())
		select {
		case <-ctx.Done():
			return fmt.Errorf("oidc discovery never succeeded: %w", err)
		case <-time.After(2 * time.Second):
		}
	}
}

// Ready reports whether discovery has completed, for the readiness check.
func (v *Verifier) Ready(context.Context) error {
	if v.verifier.Load() == nil {
		return errors.New("oidc discovery pending")
	}
	return nil
}

// claims are the token fields the service relies on. Keycloak puts realm
// roles under realm_access and the client id under azp.
type claims struct {
	Subject     string `json:"sub"`
	ClientID    string `json:"azp"`
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// Verify checks signature, issuer, audience and expiry, then extracts the
// principal. Any failure is reported as auth.ErrUnauthenticated so callers
// never leak which check failed.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (auth.Principal, error) {
	verifier := v.verifier.Load()
	if verifier == nil {
		return auth.Principal{}, fmt.Errorf("%w: verifier not initialized", auth.ErrUnauthenticated)
	}
	token, err := verifier.Verify(ctx, rawToken)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("%w: %v", auth.ErrUnauthenticated, err)
	}

	var c claims
	if err := token.Claims(&c); err != nil {
		return auth.Principal{}, fmt.Errorf("%w: malformed claims", auth.ErrUnauthenticated)
	}
	// The provider claim name is configurable, so it is read dynamically.
	var custom map[string]any
	if err := token.Claims(&custom); err != nil {
		return auth.Principal{}, fmt.Errorf("%w: malformed claims", auth.ErrUnauthenticated)
	}
	providerID, _ := custom[v.cfg.ProviderClaim].(string)

	return auth.Principal{
		Subject: c.Subject, ClientID: c.ClientID, Roles: c.RealmAccess.Roles, ProviderID: providerID,
	}, nil
}
