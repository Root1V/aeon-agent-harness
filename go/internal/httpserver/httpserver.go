// Package httpserver provides the minimal shared HTTP scaffolding (health checks, readiness)
// used by every Go binary in this repo (control plane, model gateway, tool gateway). Business
// logic for each service lives in its own internal package, not here.
package httpserver

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

// New builds an http.Server exposing /healthz and /readyz, the minimum every Aeon Go service
// must serve so compose healthchecks and k8s probes have something real to check.
func New(serviceName string, mux *http.ServeMux) *http.Server {
	if mux == nil {
		mux = http.NewServeMux()
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"service": serviceName, "status": "ok"})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return &http.Server{Addr: ":" + Port(), Handler: mux}
}

// Port reads AEON_PORT from the environment, defaulting to 8090. Individual binaries override
// this default via their own env var (e.g. AEON_CONTROLPLANE_PORT) before calling New.
func Port() string {
	if p := os.Getenv("AEON_PORT"); p != "" {
		return p
	}
	return "8090"
}

// MustListenAndServe is a small convenience wrapper so cmd/ mains stay one-liners.
func MustListenAndServe(srv *http.Server) {
	log.Printf("listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
