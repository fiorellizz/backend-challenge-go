package fxmodules

import (
	"log/slog"
	"net/http"

	"go.uber.org/fx"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/httpapi"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
)

// HTTP runs the server on every instance, whatever its roles: health
// checks must answer even on a consumer-only process. Business routes are
// mounted only when the "api" role is enabled.
var HTTP = fx.Module("httpapi",
	fx.Provide(
		httpapi.NewMux,
		httpapi.NewServer,
		httpapi.NewWalletHandler,
		// NewHealth receives every ReadinessCheck contributed to the
		// "readiness" group by the adapters (PostgreSQL, SQS).
		fx.Annotate(httpapi.NewHealth, fx.ParamTags(`group:"readiness"`)),
	),
	fx.Invoke(registerHealth, registerAPIRoutes, runServer),
)

func registerHealth(mux *http.ServeMux, h *httpapi.Health) {
	h.Register(mux)
}

// registerAPIRoutes mounts the business endpoints when this instance
// serves the API. Consumer-only and outbox-only instances skip them.
func registerAPIRoutes(cfg config.Config, log *slog.Logger, mux *http.ServeMux, wallets *httpapi.WalletHandler) {
	if !cfg.Roles.Has(config.RoleAPI) {
		log.Info("api role disabled; business routes not mounted")
		return
	}
	wallets.Register(mux)
}

// runServer ties the server to the container lifecycle. Fx stops hooks in
// reverse registration order and App lists this module last, so the server
// is the first component to stop: it refuses new requests while workers
// finish and before the connection pool closes.
func runServer(lc fx.Lifecycle, srv *httpapi.Server) {
	lc.Append(fx.Hook{
		OnStart: srv.Start,
		OnStop:  srv.Stop,
	})
}
