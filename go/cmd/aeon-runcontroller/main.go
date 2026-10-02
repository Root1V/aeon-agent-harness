// Command aeon-runcontroller is the Run Controller (RUN-001): start/cancel/pause/resume/status/
// stream for a run, as a thin HTTP layer over the Temporal client. It holds no run state of its
// own — Temporal is the source of truth (docs/adr/0001) — so this service is stateless and can
// restart or scale freely, with one optional exception: A5's circuit breaker enforcement. Given
// AEON_PG_DSN, a POST /runs naming agent_manifest_ref for a quarantined version is refused before
// ever reaching Temporal (go/internal/api's checkNotQuarantined) — without it, this check is simply
// skipped, exactly like before A5 existed. Also mounts OBS-002's Agent Console
// (GET /console/runs/{run_id}), a real HTML page combining this same Controller.Status with a real
// Tempo query (AEON_TEMPO_QUERY_URL, optional — empty just disables the trace panel).
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"

	"github.com/aeon-ai/aeon/go/internal/api"
	"github.com/aeon-ai/aeon/go/internal/auth"
	"github.com/aeon-ai/aeon/go/internal/checkpoint"
	"github.com/aeon-ai/aeon/go/internal/httpserver"
	"github.com/aeon-ai/aeon/go/internal/runcontroller"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/tracing"
)

func main() {
	if p := os.Getenv("AEON_RUNCONTROLLER_PORT"); p != "" {
		os.Setenv("AEON_PORT", p)
	} else {
		os.Setenv("AEON_PORT", "9404")
	}

	otelEndpoint := os.Getenv("AEON_OTEL_ENDPOINT")
	if otelEndpoint == "" {
		otelEndpoint = "otel-collector:4318"
	}
	if _, shutdown, err := tracing.Init(context.Background(), "runcontroller", "api", otelEndpoint); err != nil {
		log.Printf("aeon-runcontroller: tracing disabled: %v", err)
	} else {
		defer shutdown(context.Background())
	}

	address := os.Getenv("AEON_TEMPORAL_ADDRESS")
	if address == "" {
		address = "localhost:7233"
	}
	namespace := os.Getenv("AEON_TEMPORAL_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}
	taskQueue := os.Getenv("AEON_TASK_QUEUE") // defaults to runcontroller.DefaultTaskQueue when empty

	// OBS-010b: the interceptor that carries this process's trace context into the workflow, so a run
	// started over HTTP is ONE trace from the request down to the worker's Activities. Without it the
	// spans all existed and none of them were connected — see tracing.TemporalInterceptor.
	temporalClient, err := client.Dial(client.Options{
		HostPort:     address,
		Namespace:    namespace,
		Interceptors: []interceptor.ClientInterceptor{tracing.TemporalInterceptor()},
	})
	if err != nil {
		log.Fatalf("aeon-runcontroller: connecting to Temporal at %s: %v", address, err)
	}
	defer temporalClient.Close()

	var registry *store.AgentRegistry
	var checkpointer checkpoint.Checkpointer
	// MDL-018: where a run's real cost and token count come from. Without it the status endpoint omits
	// them instead of reporting zeros.
	var ledger *store.FinOpsLedger
	if dsn := os.Getenv("AEON_PG_DSN"); dsn != "" {
		s, err := store.Connect(context.Background(), dsn)
		if err != nil {
			log.Fatalf("aeon-runcontroller: connecting to Postgres for the circuit breaker check: %v", err)
		}
		defer s.Close()
		registry = s.AgentRegistry()
		log.Println("aeon-runcontroller: circuit breaker enforcement live (a quarantined agent_manifest_ref will be refused)")

		// INT-011: a person's approval decision is journalled as a known outcome, because the person decides
		// while the loop is not running and only this process witnesses it.
		checkpointer = s.Checkpointer()
		log.Println("aeon-runcontroller: approval decisions journalled as known outcomes (INT-011)")

		ledger = s.FinOpsLedger()
		log.Println("aeon-runcontroller: GET /runs/{id} reports real cost and tokens from the FinOps ledger (MDL-018)")
	} else {
		log.Println("aeon-runcontroller: AEON_PG_DSN not set — circuit breaker enforcement skipped (see A5 in roadmap.md), approval decisions not journalled (INT-011), and run status OMITS cost/tokens rather than reporting zeros (MDL-018)")
	}

	controller := runcontroller.New(temporalClient, taskQueue)
	controller.Ledger = ledger
	mux := http.NewServeMux()
	handlers := &api.RunControllerHandlers{
		Controller:   controller,
		Registry:     registry,
		Checkpointer: checkpointer,
	}
	handlers.Register(mux)

	// OBS-002: the Agent Console's trace explorer. TempoURL empty simply disables the trace panel
	// (run status still renders) — optional like everything else here.
	tempoURL := os.Getenv("AEON_TEMPO_QUERY_URL")
	(&api.ConsoleHandlers{Controller: controller, TempoURL: tempoURL}).Register(mux)

	// SEC-005: every route but /healthz and /readyz is behind this. Fatal when unconfigured — see
	// auth.MustLoadFromEnv for why a warning would be worse than not starting.
	callers := auth.MustLoadFromEnv("aeon-runcontroller")
	srv := httpserver.New("aeon-runcontroller", mux, callers)
	log.Printf("aeon-runcontroller starting (temporal=%s, task_queue=%s)", address, taskQueue)
	httpserver.MustListenAndServe(srv)
}
