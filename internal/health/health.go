// Package health serves /healthz (the process is alive) and /readyz (the process can do its work).
package health

import (
	"context"
	"net/http"
	"sync"
)

// Check reports whether a dependency is ready; nil means ready.
type Check func(ctx context.Context) error

// Handler serves the health endpoints. Readiness is the conjunction of every registered check.
type Handler struct {
	mu     sync.RWMutex
	checks map[string]Check
}

// New returns a Handler with no checks: ready until one is added.
func New() *Handler { return &Handler{checks: map[string]Check{}} }

// Add registers (or replaces) a named readiness check, e.g. "jiku-bus" or "github".
func (h *Handler) Add(name string, c Check) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checks[name] = c
}

// Ready runs every check and returns the name and error of the first one that fails.
func (h *Handler) Ready(ctx context.Context) (string, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for name, c := range h.checks {
		if err := c(ctx); err != nil {
			return name, err
		}
	}
	return "", nil
}

// Mux returns a ServeMux with /healthz and /readyz.
func (h *Handler) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if name, err := h.Ready(r.Context()); err != nil {
			http.Error(w, name+": not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	return mux
}
