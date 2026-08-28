// Command aeon-toolgw is the Tool Gateway (TOOL-001): executes typed tool calls after the Policy
// Engine has checked them (post-argument-generation, pre-execution — see docs/adr/0001), enforces
// idempotency via a dedupe table keyed on idempotency_key (RUN-004), and hosts the MCP
// client/server adapters (TOOL-002).
//
// STATUS: scaffolding only. Policy checks, the dedupe table, sandboxing, and MCP adapters are not
// yet implemented — see roadmap.md TOOL-001..003, SEC-001, all currently `TODO`.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/aeon-ai/aeon/go/internal/httpserver"
)

func main() {
	if p := os.Getenv("AEON_TOOLGW_PORT"); p != "" {
		os.Setenv("AEON_PORT", p)
	} else {
		os.Setenv("AEON_PORT", "9403")
	}
	mux := http.NewServeMux()
	srv := httpserver.New("aeon-toolgw", mux)
	log.Println("aeon-toolgw starting (policy checks, dedupe table, MCP adapters not yet implemented — see roadmap.md TOOL-001..003)")
	httpserver.MustListenAndServe(srv)
}
