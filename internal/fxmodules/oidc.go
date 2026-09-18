package fxmodules

import (
	"log/slog"

	"go.uber.org/fx"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/httpapi"
	"github.com/fiorellizz/backend-challenge-go/internal/adapter/oidc"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/auth"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
)

// OIDC provides the token verifier. Discovery runs in OnStart, retrying
// while the IdP boots, and the hook is registered in the constructor so it
// completes before the HTTP server starts accepting requests.
var OIDC = fx.Module("oidc",
	fx.Provide(
		newVerifier,
		func(v *oidc.Verifier) auth.Verifier { return v },
		fx.Annotate(oidcReadiness, fx.ResultTags(`group:"readiness"`)),
	),
)

func newVerifier(lc fx.Lifecycle, cfg config.Config, log *slog.Logger) *oidc.Verifier {
	verifier := oidc.NewVerifier(cfg, log)
	lc.Append(fx.Hook{OnStart: verifier.Init})
	return verifier
}

func oidcReadiness(v *oidc.Verifier) httpapi.ReadinessCheck {
	return httpapi.ReadinessCheck{Name: "oidc", Check: v.Ready}
}
