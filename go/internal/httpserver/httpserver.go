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

	"github.com/aeon-ai/aeon/go/internal/auth"
)

// New builds an http.Server exposing /healthz and /readyz, the minimum every Aeon Go service
// must serve so compose healthchecks and k8s probes have something real to check, with every OTHER
// route behind authentication (SEC-005).
//
// AUTH GOES HERE FOR THE SAME REASON TRACE EXTRACTION DOES: it is the one place every binary builds
// its server, so a new endpoint is protected by existing rather than by remembering. The version of
// this that took an optional authenticator was written first and thrown away — an optional guard on a
// security boundary is a guard that is off in whichever deployment nobody checked.
//
// A NIL AUTHENTICATOR PANICS, deliberately and at startup. It cannot be a runtime 500: a service that
// starts and serves unauthenticated requests while logging a warning is exactly the state SEC-005
// exists to end, and the warning is read by nobody. Each binary already log.Fatals on its missing
// config before reaching here, so this panic is the backstop for a wiring mistake, not the operator's
// error message.
//
// HEALTH STAYS OPEN, and that is a decision rather than an oversight. A compose healthcheck and a
// kubelet probe hold no credential, and the two handlers answer a fixed string and a 200 — they
// disclose that the process is up, which anyone who can reach the port already knows from the TCP
// connection. Nothing else is registered outside the wrapped mux.
func New(serviceName string, mux *http.ServeMux, callers *auth.Authenticator) *http.Server {
	if callers == nil {
		panic("httpserver: New requires an *auth.Authenticator — load one from AEON_CALLERS_PATH (SEC-005)")
	}
	if mux == nil {
		mux = http.NewServeMux()
	}
	// The probes are registered on a SEPARATE mux so they sit outside auth.Require. Registering them on
	// `mux` and then exempting their paths inside the middleware would work and would put the exemption
	// list one edit away from growing.
	top := http.NewServeMux()
	top.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"service": serviceName, "status": "ok"})
	})
	top.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	top.Handle("/", auth.Require(callers)(mux))

	// Trace context is extracted for EVERY request of every binary, here, once. Doing it per-handler
	// would mean each new endpoint has to remember, and the ones that forgot would silently start their
	// own trace — which is the failure this whole change is fixing, reintroduced one handler at a time.
	//
	// OUTSIDE the auth wrapper, so a 401 is still part of the caller's trace. A rejected request is one
	// of the things an operator most wants to follow, and extracting the context only after the guard
	// would make every refusal a root span of its own.
	return &http.Server{Addr: ":" + Port(), Handler: ExtractTraceContext(top)}
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
