package fxmodules

import (
	"net/http"

	"go.uber.org/fx"

	"github.com/fiorellizz/backend-challenge-go/internal/adapter/httpapi"
)

// HTTP runs the server on every instance, whatever its roles: health
// checks must answer even on a consumer-only process. Business routes are
// mounted by their own modules when the "api" role is enabled.
var HTTP = fx.Module("httpapi",
	fx.Provide(
		httpapi.NewMux,
		httpapi.NewServer,
		// NewHealth receives every ReadinessCheck contributed to the
		// "readiness" group by the adapters (PostgreSQL, SQS).
		fx.Annotate(httpapi.NewHealth, fx.ParamTags(`group:"readiness"`)),
	),
	fx.Invoke(registerHealth, runServer),
)

func registerHealth(mux *http.ServeMux, h *httpapi.Health) {
	h.Register(mux)
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
