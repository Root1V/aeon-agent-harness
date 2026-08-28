// Command aeon-controlplane serves the Agent/Tool/Prompt/Skill/Eval/Policy registries, approvals,
// ABOM generation and release gates (FND-001, RUN-005, EVAL-003 — see roadmap.md F0/F2/F4).
//
// STATUS: the Agent Registry (FND-001) and Tool Registry (TOOL-001 CRUD portion) are real,
// Postgres-backed, and exposed over HTTP — see go/internal/store and go/internal/api. Cedar policy
// evaluation (ADR-002), approvals (RUN-005) and ABOM (FND-002) are not yet implemented — see
// roadmap.md, still `TODO`.
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

	mux := http.NewServeMux()
	handlers := &api.RegistryHandlers{Store: s}
	handlers.Register(mux)

	srv := httpserver.New("aeon-controlplane", mux)
	log.Println("aeon-controlplane starting (Agent/Tool registries live; policy/approvals/ABOM not yet implemented — see roadmap.md F0/F4)")
	httpserver.MustListenAndServe(srv)
}
