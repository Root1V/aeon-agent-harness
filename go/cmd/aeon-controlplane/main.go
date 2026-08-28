// Command aeon-controlplane serves the Agent/Tool/Prompt/Skill/Eval/Policy registries, approvals,
// ABOM generation and release gates (FND-001, RUN-005, EVAL-003 — see roadmap.md F0/F2/F4).
//
// STATUS: scaffolding only. It serves health/readiness so `make dev` produces a stack where every
// container is actually healthy, but the registry CRUD, Cedar policy evaluation (ADR-002), and
// approval endpoints described in the architecture are not yet implemented — see roadmap.md
// FND-001, SEC-001, RUN-005, all currently `TODO`.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/aeon-ai/aeon/go/internal/httpserver"
)

func main() {
	if p := os.Getenv("AEON_CONTROLPLANE_PORT"); p != "" {
		os.Setenv("AEON_PORT", p)
	} else {
		os.Setenv("AEON_PORT", "9401")
	}
	mux := http.NewServeMux()
	srv := httpserver.New("aeon-controlplane", mux)
	log.Println("aeon-controlplane starting (registries/policy/approvals not yet implemented — see roadmap.md F0)")
	httpserver.MustListenAndServe(srv)
}
