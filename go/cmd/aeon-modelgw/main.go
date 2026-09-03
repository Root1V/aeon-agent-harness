// Command aeon-modelgw is the Model Gateway (MDL-001): the only component in the platform allowed
// to talk to a model provider directly. It resolves a capability profile to a concrete
// provider/model pair via a ModelPolicyBundle, tries candidates in priority order with fallback,
// and exposes that over HTTP: POST /decide (go/internal/api/model_gateway_handlers.go, DR-001's
// own Model Gateway contract) and POST /v1/chat/completions (go/internal/api/
// openai_compatible_handlers.go, INT-002/Modo C — a real OpenAI-wire-format endpoint any existing
// framework can point its base_url at). See docs/adr/0004-model-gateway-provider-abstraction.md.
//
// Each of the 5 adapters (go/internal/providers/*) is registered only when its required
// configuration is present in the environment (see .env.example) — a candidate naming an
// unconfigured provider fails over to the next candidate (modelgateway.Gateway's normal fallback),
// rather than this process refusing to start.
//
// Also hosts OBS-003's FinOps: real per-call cost computation (from the same ModelPolicyBundle's
// candidates' cost_model/cost_per_million_*_tokens fields) and, given AEON_PG_DSN, a durable cost
// ledger plus its GET /finops/costs dashboard — both optional, like the rest of this binary's
// config-as-code inputs.
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/aeon-ai/aeon/go/internal/api"
	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/httpserver"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/providers/anthropic"
	"github.com/aeon-ai/aeon/go/internal/providers/gemini"
	"github.com/aeon-ai/aeon/go/internal/providers/openai"
	openaicompatible "github.com/aeon-ai/aeon/go/internal/providers/openai_compatible"
	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/tracing"
)

func main() {
	if p := os.Getenv("AEON_MODELGW_PORT"); p != "" {
		os.Setenv("AEON_PORT", p)
	} else {
		os.Setenv("AEON_PORT", "9402")
	}

	otelEndpoint := os.Getenv("AEON_OTEL_ENDPOINT")
	if otelEndpoint == "" {
		otelEndpoint = "otel-collector:4318"
	}
	if _, shutdown, err := tracing.Init(context.Background(), "aeon-modelgw", otelEndpoint); err != nil {
		log.Printf("aeon-modelgw: tracing disabled: %v", err)
	} else {
		defer shutdown(context.Background())
	}

	gw := modelgateway.New()
	registered := registerProvidersFromEnv(gw)
	log.Printf("aeon-modelgw: registered providers: %v", registered)

	bundle := loadModelPolicyBundle()
	pricing := finops.NewPricingTable(bundle.PricingRates())

	var ledger *store.FinOpsLedger
	if dsn := os.Getenv("AEON_PG_DSN"); dsn != "" {
		s, err := store.Connect(context.Background(), dsn)
		if err != nil {
			log.Fatalf("aeon-modelgw: connecting to Postgres for the FinOps ledger: %v", err)
		}
		defer s.Close()
		ledger = s.FinOpsLedger()
		log.Println("aeon-modelgw: FinOps cost ledger live (GET /finops/costs)")
	} else {
		log.Println("aeon-modelgw: AEON_PG_DSN not set — FinOps costs computed per-call but not durably recorded, /finops/costs not mounted")
	}

	mux := http.NewServeMux()
	(&api.ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: ledger}).Register(mux)
	(&api.OpenAICompatibleHandlers{Gateway: gw, Bundle: bundle}).Register(mux)
	if ledger != nil {
		(&api.FinOpsHandlers{Ledger: ledger}).Register(mux)
	}

	srv := httpserver.New("aeon-modelgw", mux)
	log.Println("aeon-modelgw starting")
	httpserver.MustListenAndServe(srv)
}

// loadModelPolicyBundle loads the real, config-as-code ModelPolicyBundle (FND-003) that
// POST /v1/chat/completions (INT-002) resolves profile names against. Optional: without
// AEON_MODEL_POLICY_BUNDLE_PATH set, /decide still works exactly as before — only the
// OpenAI-compatible endpoint is affected, and it reports a clear "profile not found" for every
// request until one is configured, rather than refusing to start.
func loadModelPolicyBundle() modelgateway.ModelPolicyBundleDoc {
	path := os.Getenv("AEON_MODEL_POLICY_BUNDLE_PATH")
	if path == "" {
		log.Println("aeon-modelgw: AEON_MODEL_POLICY_BUNDLE_PATH not set — /v1/chat/completions will report every profile as not found until one is configured")
		return modelgateway.ModelPolicyBundleDoc{}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("aeon-modelgw: reading ModelPolicyBundle %s: %v", path, err)
	}
	var doc modelgateway.ModelPolicyBundleDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		log.Fatalf("aeon-modelgw: parsing ModelPolicyBundle %s: %v", path, err)
	}
	log.Printf("aeon-modelgw: loaded %d profile(s) from ModelPolicyBundle %s", len(doc.Profiles), path)
	return doc
}

// registerProvidersFromEnv wires each adapter whose required credentials/endpoints are present in
// the environment (see .env.example) and returns the names actually registered.
func registerProvidersFromEnv(gw *modelgateway.Gateway) []string {
	var registered []string

	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		gw.RegisterProvider(anthropic.Name, &anthropic.Adapter{APIKey: key})
		registered = append(registered, anthropic.Name)
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		gw.RegisterProvider(openai.Name, &openai.Adapter{APIKey: key})
		registered = append(registered, openai.Name)
	}
	if key := os.Getenv("GOOGLE_API_KEY"); key != "" {
		gw.RegisterProvider(gemini.Name, &gemini.Adapter{APIKey: key})
		registered = append(registered, gemini.Name)
	}
	if clientID := os.Getenv("PROMETHEUS_CLIENT_ID"); clientID != "" {
		gw.RegisterProvider(prometheusinference.Name, prometheusinference.New(
			os.Getenv("PROMETHEUS_AUTH_URL"),
			os.Getenv("PROMETHEUS_GATEWAY_URL"),
			clientID,
			os.Getenv("PROMETHEUS_CLIENT_SECRET"),
			os.Getenv("PROMETHEUS_SCOPE"),
			os.Getenv("PROMETHEUS_DEFAULT_MODEL"),
		))
		registered = append(registered, prometheusinference.Name)
	}
	if baseURL := os.Getenv("OPENAI_COMPATIBLE_BASE_URL"); baseURL != "" {
		gw.RegisterProvider(openaicompatible.Name, &openaicompatible.Adapter{
			BaseURL: baseURL,
			APIKey:  os.Getenv("OPENAI_COMPATIBLE_API_KEY"),
		})
		registered = append(registered, openaicompatible.Name)
	}

	return registered
}
