package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/app/config"
)

// Server encapsulates the HTTP server for the Checkpoint Intelligence Backend API.
type Server struct {
	cfg        *config.Config
	deps       *ServerDependencies
	httpServer *http.Server
}

// loggingMiddleware wraps an http.Handler to provide structured request logging.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("HTTP Request",
			"method", r.Method,
			"path", r.URL.Path,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// NewServer constructs a new API Server.
func NewServer(cfg *config.Config, deps *ServerDependencies) *Server {
	if cfg == nil {
		cfg = config.LoadConfig()
	}
	if deps == nil {
		deps = DefaultServerDependencies()
	}

	apiHandler := NewAPIHandler(deps)
	mux := http.NewServeMux()

	// API Endpoints
	mux.HandleFunc("/api/health", apiHandler.HealthHandler)
	mux.HandleFunc("/api/readiness", apiHandler.ReadinessHandler)
	mux.HandleFunc("/api/enable", apiHandler.EnableHandler)
	mux.HandleFunc("/api/repositories", apiHandler.RepositoriesHandler)
	mux.HandleFunc("/api/repositories/", apiHandler.RepositoriesHandler)

	// Static Frontend Dashboard File Server
	frontendDir := filepath.Join(".", "app", "frontend")
	if _, err := os.Stat(frontendDir); err == nil {
		fs := http.FileServer(http.Dir(frontendDir))
		mux.Handle("/", fs)
	}

	addr := fmt.Sprintf("%s:%s", cfg.ServerHost, cfg.ServerPort)
	srv := &http.Server{
		Addr:    addr,
		Handler: loggingMiddleware(mux),
	}

	return &Server{
		cfg:        cfg,
		deps:       deps,
		httpServer: srv,
	}
}

// Start runs the HTTP server asynchronously.
func (s *Server) Start() error {
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
