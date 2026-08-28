// Command aeon-modelgw is the Model Gateway (MDL-001): the only component in the platform allowed
// to talk to a model provider directly. It resolves a capability profile to a concrete
// provider/model pair via a ModelPolicyBundle, enforces budgets, and returns a typed Decision
// (proto/schemas/decision.schema.json) — never raw provider text interpreted as a command
// downstream. See docs/adr/0004-model-gateway-provider-abstraction.md.
//
// STATUS: scaffolding only. Provider adapters (go/internal/providers/*) are stubs; routing,
// fallback, and the provider_conformance suite are not yet implemented — see roadmap.md
// MDL-001..007, all currently `TODO`.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/aeon-ai/aeon/go/internal/httpserver"
)

func main() {
	if p := os.Getenv("AEON_MODELGW_PORT"); p != "" {
		os.Setenv("AEON_PORT", p)
	} else {
		os.Setenv("AEON_PORT", "9402")
	}
	mux := http.NewServeMux()
	srv := httpserver.New("aeon-modelgw", mux)
	log.Println("aeon-modelgw starting (provider adapters not yet implemented — see roadmap.md MDL-001..007)")
	httpserver.MustListenAndServe(srv)
}
