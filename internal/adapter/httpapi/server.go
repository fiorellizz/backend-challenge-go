// Package httpapi exposes the service over HTTP: routing, the server
// lifecycle, health checks and, in the feature files, the business handlers.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/fiorellizz/backend-challenge-go/internal/platform/config"
)

// Server wraps http.Server with a lifecycle that binds the port
// synchronously (so a busy port fails the boot) and serves in the
// background until Stop drains in-flight requests.
type Server struct {
	http *http.Server
	addr string
	ln   net.Listener
	log  *slog.Logger
}

// NewMux returns the router every feature registers its routes on.
func NewMux() *http.ServeMux { return http.NewServeMux() }

// NewServer builds the server around the shared mux.
func NewServer(cfg config.Config, mux *http.ServeMux, log *slog.Logger) *Server {
	return &Server{
		addr: cfg.HTTPAddr,
		log:  log.With("component", "http"),
		http: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
	}
}

// Start binds the listener and serves in a goroutine. It returns as soon
// as the port is bound; it never blocks the lifecycle.
func (s *Server) Start(ctx context.Context) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", s.addr, err)
	}
	s.ln = ln
	s.log.Info("http server listening", "addr", ln.Addr().String())

	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("http server stopped unexpectedly", "error", err)
		}
	}()
	return nil
}

// Stop refuses new connections and waits for in-flight requests until ctx
// expires, then closes whatever is left.
func (s *Server) Stop(ctx context.Context) error {
	s.log.Info("http server shutting down")
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	return nil
}

// Addr returns the bound address, useful when HTTP_ADDR ends in ":0".
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.addr
	}
	return s.ln.Addr().String()
}
