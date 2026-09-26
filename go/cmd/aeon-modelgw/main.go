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
// config-as-code inputs. The same AEON_PG_DSN also enables MDL-002's quality-aware routing: a real
// eval score reported via POST /quality-scores can make Decide skip a degraded candidate entirely.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

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

	// The bundle is loaded BEFORE the providers, and the order is the fix rather than a tidy-up
	// (MDL-015): Prometheus issues per-model OAuth scopes, so the token has to name every model this
	// gateway may route to, and only the bundle knows which those are.
	bundle := loadModelPolicyBundle()
	pricing := finops.NewPricingTable(bundle.PricingRates())

	gw := modelgateway.New()
	registered := registerProvidersFromEnv(gw, bundle)
	log.Printf("aeon-modelgw: registered providers: %v", registered)

	var ledger *store.FinOpsLedger
	var qualityScores *store.QualityScoreStore
	if dsn := os.Getenv("AEON_PG_DSN"); dsn != "" {
		s, err := store.Connect(context.Background(), dsn)
		if err != nil {
			log.Fatalf("aeon-modelgw: connecting to Postgres for the FinOps ledger / quality scores: %v", err)
		}
		defer s.Close()
		ledger = s.FinOpsLedger()
		log.Println("aeon-modelgw: FinOps cost ledger live (GET /finops/costs)")

		qualityScores = s.QualityScores(qualityScoreThreshold())
		gw.Quality = qualityScores
		log.Printf("aeon-modelgw: quality-aware routing live (MDL-002), degraded threshold=%.2f", qualityScoreThreshold())
	} else {
		log.Println("aeon-modelgw: AEON_PG_DSN not set — FinOps costs computed per-call but not durably recorded, and quality-aware routing (MDL-002) disabled")
	}

	mux := http.NewServeMux()
	(&api.ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: ledger}).Register(mux)
	(&api.OpenAICompatibleHandlers{Gateway: gw, Bundle: bundle}).Register(mux)
	if ledger != nil {
		(&api.FinOpsHandlers{Ledger: ledger}).Register(mux)
	}
	if qualityScores != nil {
		(&api.QualityScoreHandlers{Scores: qualityScores}).Register(mux)
	}

	srv := httpserver.New("aeon-modelgw", mux)
	log.Println("aeon-modelgw starting")
	httpserver.MustListenAndServe(srv)
}

// qualityScoreThreshold is MDL-002's degraded-below cutoff (AEON_QUALITY_SCORE_THRESHOLD, default
// 0.5) — a candidate whose most recently reported eval score is below this is skipped by routing.
func qualityScoreThreshold() float64 {
	raw := os.Getenv("AEON_QUALITY_SCORE_THRESHOLD")
	if raw == "" {
		return 0.5
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		log.Fatalf("aeon-modelgw: AEON_QUALITY_SCORE_THRESHOLD %q is not a valid number: %v", raw, err)
	}
	return v
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
func registerProvidersFromEnv(gw *modelgateway.Gateway, bundle modelgateway.ModelPolicyBundleDoc) []string {
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
			prometheusScope(bundle),
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

// prometheusScope builds the OAuth scope the Prometheus adapter requests, naming EVERY
// prometheus_inference model the bundle declares (MDL-015).
//
// The bug this fixes had no symptom until the pipeline ran against the real platform. PROMETHEUS_SCOPE
// was a hand-written string naming ONE model, while the model is chosen per request by routing — so
// the gateway could only ever serve that one, and every other candidate came back:
//
//	403 "This client is not authorized to use model 'qwen3-0.6b'"
//
// Which made the bundle's declared fallback chain unusable: two prometheus_inference candidates at
// priorities 0 and 1, and falling back to the second would have failed in production. MDL-006 tests
// the adapter with one model and DX-001 tests the pipeline against a double, so nothing looked at the
// chain against a platform that enforces scopes.
//
// Measured before relying on it: one token really can carry several model scopes — requesting
// "inference:read model:gpt-oss-20b-mxfp4 model:qwen3-0.6b" is granted verbatim. So this is one token
// for all candidates, not a token per call.
//
// PROMETHEUS_SCOPE still wins when set, for a deployment that must pin the scope by hand; it just
// stops being the only source, since a hand-written list silently goes stale the moment someone adds
// a candidate to the bundle.
func prometheusScope(bundle modelgateway.ModelPolicyBundleDoc) string {
	if explicit := os.Getenv("PROMETHEUS_SCOPE"); explicit != "" {
		log.Printf("aeon-modelgw: using PROMETHEUS_SCOPE from the environment, not the bundle: %q", explicit)
		return explicit
	}

	scopes := []string{"inference:read", "inference:stream"}
	seen := map[string]bool{}
	for _, profile := range bundle.Profiles {
		for _, candidate := range profile.Candidates {
			if candidate.Provider != prometheusinference.Name || candidate.Model == "" || seen[candidate.Model] {
				continue
			}
			seen[candidate.Model] = true
			scopes = append(scopes, "model:"+candidate.Model)
		}
	}
	scope := strings.Join(scopes, " ")
	log.Printf("aeon-modelgw: Prometheus scope derived from the bundle (%d model(s)): %q", len(seen), scope)
	return scope
}
