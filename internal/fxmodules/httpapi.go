package fxmodules

import (
	"log/slog"
	"net/http"

	"go.uber.org/fx"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/httpapi"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
	"github.com/fiorellizz/backend-challenge-go/internal/platform/metrics"
)

// HTTP runs the server on every instance, whatever its roles: health
// checks must answer even on a consumer-only process. Business routes are
// mounted only when the "api" role is enabled.
var HTTP = fx.Module("httpapi",
	fx.Provide(
		httpapi.NewMux,
		newServer,
		httpapi.NewAuth,
		httpapi.NewWalletHandler,
		httpapi.NewWageringHandler,
		// NewHealth receives every ReadinessCheck contributed to the
		// "readiness" group by the adapters (PostgreSQL, SQS).
		fx.Annotate(httpapi.NewHealth, fx.ParamTags(`group:"readiness"`)),
	),
	fx.Invoke(registerHealth, registerMetrics, registerAPIRoutes, runServer),
)

// newServer wraps the router with the request middleware (correlation id,
// access log, HTTP metrics) before handing it to the server.
func newServer(cfg config.Config, mux *http.ServeMux, m *metrics.Metrics, log *slog.Logger) *httpapi.Server {
	return httpapi.NewServer(cfg, httpapi.Instrument(mux, m, log), log)
}

func registerHealth(mux *http.ServeMux, h *httpapi.Health) {
	h.Register(mux)
}

// registerMetrics exposes /metrics publicly on every instance.
func registerMetrics(mux *http.ServeMux, m *metrics.Metrics) {
	mux.Handle("GET /metrics", m.Handler())
}

// registerAPIRoutes mounts the business endpoints when this instance
// serves the API. Consumer-only and outbox-only instances skip them.
func registerAPIRoutes(cfg config.Config, log *slog.Logger, mux *http.ServeMux, guard *httpapi.Auth, wallets *httpapi.WalletHandler, wagering *httpapi.WageringHandler) {
	if !cfg.Roles.Has(config.RoleAPI) {
		log.Info("api role disabled; business routes not mounted")
		return
	}
	wallets.Register(mux, guard)
	wagering.Register(mux, guard)
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
