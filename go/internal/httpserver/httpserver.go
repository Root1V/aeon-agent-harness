// Package httpserver provides the minimal shared HTTP scaffolding (health checks, readiness)
// used by every Go binary in this repo (control plane, model gateway, tool gateway). Business
// logic for each service lives in its own internal package, not here.
package httpserver

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

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
	// Trace context is extracted for EVERY request of every binary, here, once. Doing it per-handler
	// would mean each new endpoint has to remember, and the ones that forgot would silently start their
	// own trace — which is the failure this whole change is fixing, reintroduced one handler at a time.
	return &http.Server{Addr: ":" + Port(), Handler: ExtractTraceContext(mux)}
}

// ExtractTraceContext reads W3C traceparent/baggage off the request and puts it in the request context,
// so a span started by a handler becomes a CHILD of the caller's span instead of a new root.
//
// A plain wrapper rather than otelhttp.NewHandler: otelhttp would also create a server span per request,
// named after the route, and every handler here already creates its own span with OTel GenAI semantics
// (`chat`, `execute_tool`, `invoke_agent`). Two spans per request where one is meaningful would double the
// cold-path volume to buy a name we already have.
func ExtractTraceContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
