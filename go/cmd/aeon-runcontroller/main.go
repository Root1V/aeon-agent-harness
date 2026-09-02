// Command aeon-runcontroller is the Run Controller (RUN-001): start/cancel/pause/resume/status/
// stream for a run, as a thin HTTP layer over the Temporal client. It holds no run state of its
// own — Temporal is the source of truth (docs/adr/0001) — so this service is stateless and can
// restart or scale freely, with one optional exception: A5's circuit breaker enforcement. Given
// AEON_PG_DSN, a POST /runs naming agent_manifest_ref for a quarantined version is refused before
// ever reaching Temporal (go/internal/api's checkNotQuarantined) — without it, this check is simply
// skipped, exactly like before A5 existed.
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"go.temporal.io/sdk/client"

	"github.com/aeon-ai/aeon/go/internal/api"
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
	if _, shutdown, err := tracing.Init(context.Background(), "aeon-runcontroller", otelEndpoint); err != nil {
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

	temporalClient, err := client.Dial(client.Options{HostPort: address, Namespace: namespace})
	if err != nil {
		log.Fatalf("aeon-runcontroller: connecting to Temporal at %s: %v", address, err)
	}
	defer temporalClient.Close()

	var registry *store.AgentRegistry
	if dsn := os.Getenv("AEON_PG_DSN"); dsn != "" {
		s, err := store.Connect(context.Background(), dsn)
		if err != nil {
			log.Fatalf("aeon-runcontroller: connecting to Postgres for the circuit breaker check: %v", err)
		}
		defer s.Close()
		registry = s.AgentRegistry()
		log.Println("aeon-runcontroller: circuit breaker enforcement live (a quarantined agent_manifest_ref will be refused)")
	} else {
		log.Println("aeon-runcontroller: AEON_PG_DSN not set — circuit breaker enforcement skipped (see A5 in roadmap.md)")
	}

	mux := http.NewServeMux()
	handlers := &api.RunControllerHandlers{
		Controller: runcontroller.New(temporalClient, taskQueue),
		Registry:   registry,
	}
	handlers.Register(mux)

	srv := httpserver.New("aeon-runcontroller", mux)
	log.Printf("aeon-runcontroller starting (temporal=%s, task_queue=%s)", address, taskQueue)
	httpserver.MustListenAndServe(srv)
}
