// Package logging builds the process logger. Output is structured (JSON by
// default) so log aggregators can index the correlation identifiers the
// handlers and workers attach to each record.
package logging

import (
	"io"
	"log/slog"

	"go.uber.org/fx/fxevent"

	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
)

// New builds the root logger with the instance id attached to every record.
func New(cfg config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level(cfg.Log.Level)}
	var handler slog.Handler
	if cfg.Log.Format == "text" {
		handler = slog.NewTextHandler(w, opts)
	} else {
		handler = slog.NewJSONHandler(w, opts)
	}
	return slog.New(handler).With("instance", cfg.InstanceID)
}

// ForFx routes the container's own events (provides, invokes, lifecycle
// hooks) through the application logger, so the boot sequence and the
// shutdown order show up in the same JSON stream as everything else. They
// are emitted at DEBUG to keep the default stream focused on the business;
// container errors keep the ERROR level.
func ForFx(log *slog.Logger) fxevent.Logger {
	l := &fxevent.SlogLogger{Logger: log.With("component", "fx")}
	l.UseLogLevel(slog.LevelDebug)
	l.UseErrorLevel(slog.LevelError)
	return l
}

func level(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
