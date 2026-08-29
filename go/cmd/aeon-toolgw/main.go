// Command aeon-toolgw is the Tool Gateway (TOOL-001): executes typed tool calls after the Policy
// Engine has checked them (post-argument-generation, pre-execution — see docs/adr/0001), enforces
// idempotency via a dedupe table keyed on idempotency_key (RUN-004), and hosts the MCP
// client/server adapters (TOOL-002).
//
// STATUS: the Cedar Policy Engine (SEC-001, docs/adr/0002) is real and wired to a policy-checked
// /execute endpoint — see go/internal/policy and go/internal/api. The idempotency dedupe table and
// MCP adapters (TOOL-002/TOOL-003) are not yet implemented — see roadmap.md, still `TODO`.
package main

import (
	"log"
	"net/http"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/aeon-ai/aeon/go/internal/api"
	"github.com/aeon-ai/aeon/go/internal/httpserver"
	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

func main() {
	if p := os.Getenv("AEON_TOOLGW_PORT"); p != "" {
		os.Setenv("AEON_PORT", p)
	} else {
		os.Setenv("AEON_PORT", "9403")
	}

	bundlePath := os.Getenv("AEON_POLICY_BUNDLE_PATH")
	if bundlePath == "" {
		log.Fatal("aeon-toolgw: AEON_POLICY_BUNDLE_PATH is required")
	}

	raw, err := os.ReadFile(bundlePath)
	if err != nil {
		log.Fatalf("aeon-toolgw: reading policy bundle %s: %v", bundlePath, err)
	}

	var doc policy.PolicyBundleDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		log.Fatalf("aeon-toolgw: parsing policy bundle %s: %v", bundlePath, err)
	}

	engine, err := policy.LoadEngine(doc)
	if err != nil {
		log.Fatalf("aeon-toolgw: loading Cedar policies from %s: %v", bundlePath, err)
	}
	log.Printf("aeon-toolgw: loaded %d Cedar polic(ies) from %s", len(doc.Policies), bundlePath)

	mux := http.NewServeMux()
	handlers := &api.ToolGatewayHandlers{
		Policy:   engine,
		Executor: toolexec.NewExecutor(),
	}
	handlers.Register(mux)

	srv := httpserver.New("aeon-toolgw", mux)
	log.Println("aeon-toolgw starting (policy-checked execution live; dedupe table/MCP adapters not yet implemented — see roadmap.md TOOL-002/003)")
	httpserver.MustListenAndServe(srv)
}
