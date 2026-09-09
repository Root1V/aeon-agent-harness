// Command aeon-controlplane serves the Agent/Tool/Prompt/Skill/Eval/Policy registries, approvals,
// release gates, and the circuit breaker + kill switch (FND-001, RUN-005, EVAL-003, A5 — see
// roadmap.md F0/F2/F4). ABOM generation (FND-002) lives in the `aeon` CLI (go/cmd/aeon), not here —
// it's a property of a single `aeon publish` invocation, not a service.
//
// STATUS: the Agent Registry (FND-001), Tool Registry (TOOL-001 CRUD portion), Memory Store +
// Candidate Pipeline (MEM-001/MEM-002), and the circuit breaker (A5) are real, Postgres-backed, and
// exposed over HTTP — see go/internal/store and go/internal/api. Cedar policy evaluation (ADR-002)
// and approvals (RUN-005) are not yet implemented — see roadmap.md, still `TODO`.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aeon-ai/aeon/go/internal/api"
	"github.com/aeon-ai/aeon/go/internal/circuitbreaker"
	"github.com/aeon-ai/aeon/go/internal/httpserver"
	"github.com/aeon-ai/aeon/go/internal/store"
)

func main() {
	if p := os.Getenv("AEON_CONTROLPLANE_PORT"); p != "" {
		os.Setenv("AEON_PORT", p)
	} else {
		os.Setenv("AEON_PORT", "9401")
	}

	dsn := os.Getenv("AEON_PG_DSN")
	if dsn == "" {
		log.Fatal("aeon-controlplane: AEON_PG_DSN is required")
	}
	memoryHMACKey := os.Getenv("AEON_MEMORY_HMAC_KEY")
	if memoryHMACKey == "" {
		log.Fatal("aeon-controlplane: AEON_MEMORY_HMAC_KEY is required (MEM-001 provenance_hmac)")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	connectCtx, connectCancel := context.WithTimeout(ctx, 30*time.Second)
	defer connectCancel()

	s, err := store.Connect(connectCtx, dsn)
	if err != nil {
		log.Fatalf("aeon-controlplane: connecting to Postgres: %v", err)
	}
	defer s.Close()
	log.Println("aeon-controlplane: connected to Postgres, schema migrated")

	memoryStore, err := s.MemoryStore([]byte(memoryHMACKey))
	if err != nil {
		log.Fatalf("aeon-controlplane: building memory store: %v", err)
	}

	mux := http.NewServeMux()
	handlers := &api.RegistryHandlers{Store: s}
	handlers.Register(mux)
	memoryHandlers := &api.MemoryHandlers{MemoryStore: memoryStore}
	memoryHandlers.Register(mux)

	// A5: circuit breaker + kill switch. Secrets stays nil here deliberately — aeon-toolgw's real
	// Secret Broker (SEC-002) lives in a different process, and nothing yet plumbs a cross-process
	// "revoke this agent's leases" call from here to there (see backlog.md) — quarantine still
	// applies durably to the registry and blocks new runs regardless.
	breakerHandlers := &api.CircuitBreakerHandlers{Registry: s.AgentRegistry(), Breaker: circuitbreaker.New(circuitbreaker.DefaultThresholds)}
	breakerHandlers.Register(mux)

	// INT-009: the durability seam. It lives here rather than in aeon-runcontroller because the
	// journal is persistence, and this is the process that owns Postgres — but note the consequence:
	// a framework using the seam talks to the control plane, not to the run controller.
	checkpointHandlers := &api.CheckpointHandlers{Checkpointer: s.Checkpointer()}
	checkpointHandlers.Register(mux)

	srv := httpserver.New("aeon-controlplane", mux)
	log.Println("aeon-controlplane starting (Agent/Tool registries + Memory Store/Candidate Pipeline + circuit breaker + checkpoint seam live; policy/approvals not yet implemented — see roadmap.md F0/F4)")
	httpserver.MustListenAndServe(srv)
}
