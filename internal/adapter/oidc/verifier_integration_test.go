//go:build integration

package oidc_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/oidc"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/auth"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
)

func issuer(t *testing.T) string {
	t.Helper()
	iss := os.Getenv("OIDC_ISSUER_URL")
	if iss == "" {
		t.Skip("OIDC_ISSUER_URL not set")
	}
	return iss
}

// clientCredentials obtains an access token from Keycloak the way a
// provider would: client_credentials with the client's secret.
func clientCredentials(t *testing.T, iss, clientID, secret string) string {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {secret}}
	res, err := http.PostForm(iss+"/protocol/openid-connect/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("token endpoint: %d %s", res.StatusCode, body)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out.AccessToken
}

func verifier(t *testing.T, iss string) *oidc.Verifier {
	t.Helper()
	cfg := config.Config{OIDC: config.OIDC{IssuerURL: iss, DiscoveryURL: iss, Audience: "wager-api", ProviderClaim: "provider_id"}}
	v := oidc.NewVerifier(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := v.Init(ctx); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifyRealKeycloakTokens(t *testing.T) {
	iss := issuer(t)
	v := verifier(t, iss)

	cases := []struct {
		client, secret, role, provider string
	}{
		{"provider-a", "provider-a-secret", "provider", "provider-a"},
		{"provider-b", "provider-b-secret", "provider", "provider-b"},
		{"wallet-internal", "wallet-internal-secret", "internal-service", ""},
		{"unauthorized-client", "unauthorized-client-secret", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.client, func(t *testing.T) {
			p, err := v.Verify(t.Context(), clientCredentials(t, iss, tc.client, tc.secret))
			if err != nil {
				t.Fatal(err)
			}
			if p.ClientID != tc.client || p.ProviderID != tc.provider {
				t.Fatalf("principal = %+v", p)
			}
			if tc.role != "" && !p.HasRole(tc.role) {
				t.Fatalf("missing role %s: %+v", tc.role, p)
			}
			if tc.role == "" && len(p.Roles) != 0 {
				t.Fatalf("unexpected roles: %+v", p)
			}
		})
	}
}

func TestVerifyRejectsForgedAndMalformedTokens(t *testing.T) {
	iss := issuer(t)
	v := verifier(t, iss)
	valid := clientCredentials(t, iss, "provider-a", "provider-a-secret")

	parts := strings.Split(valid, ".")
	tampered := parts[0] + "." + parts[1] + "." + strings.Repeat("A", len(parts[2]))
	swapped := parts[0] + "." + strings.Repeat("B", 40) + "." + parts[2]

	for name, raw := range map[string]string{
		"empty": "", "garbage": "not-a-jwt", "tampered signature": tampered, "tampered payload": swapped,
	} {
		_, err := v.Verify(t.Context(), raw)
		if !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

func TestVerifyRejectsTokensForAnotherAudienceOrIssuer(t *testing.T) {
	iss := issuer(t)
	valid := clientCredentials(t, iss, "provider-a", "provider-a-secret")

	other := oidc.NewVerifier(config.Config{OIDC: config.OIDC{IssuerURL: iss, DiscoveryURL: iss, Audience: "another-api", ProviderClaim: "provider_id"}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := other.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Verify(t.Context(), valid); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("wrong audience accepted: %v", err)
	}

	uninitialized := oidc.NewVerifier(config.Config{OIDC: config.OIDC{IssuerURL: iss, DiscoveryURL: iss, Audience: "wager-api"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := uninitialized.Verify(t.Context(), valid); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("uninitialized verifier accepted a token: %v", err)
	}
	if err := uninitialized.Ready(t.Context()); err == nil {
		t.Errorf("uninitialized verifier reported ready")
	}
}
