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
	"time"

	"gopkg.in/yaml.v3"

	"github.com/aeon-ai/aeon/go/internal/api"
	"github.com/aeon-ai/aeon/go/internal/auth"
	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/httpserver"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	"github.com/aeon-ai/aeon/go/internal/providers/anthropic"
	"github.com/aeon-ai/aeon/go/internal/providers/gemini"
	"github.com/aeon-ai/aeon/go/internal/providers/openai"
	openaicompatible "github.com/aeon-ai/aeon/go/internal/providers/openai_compatible"
	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
	"github.com/aeon-ai/aeon/go/internal/secretref"
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
	if _, shutdown, err := tracing.Init(context.Background(), "modelgw", "api", otelEndpoint); err != nil {
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

	verifyDeclaredModalities(bundle)

	var ledger *store.Store
	var qualityScores *store.QualityScoreStore
	// MDL-017: the agent registry, for the cost ceiling the manifest declares.
	var agents *store.Store
	if dsn := os.Getenv("AEON_PG_DSN"); dsn != "" {
		s, err := store.Connect(context.Background(), dsn)
		if err != nil {
			log.Fatalf("aeon-modelgw: connecting to Postgres for the FinOps ledger / quality scores: %v", err)
		}
		defer s.Close()
		// VRT-AEON-005: the store, with the tenant-scoped handle derived per request.
		ledger = s
		log.Println("aeon-modelgw: FinOps cost ledger live (GET /finops/costs)")
		agents = s

		qualityScores = s.QualityScores(qualityScoreThreshold())
		gw.Quality = qualityScores
		log.Printf("aeon-modelgw: quality-aware routing live (MDL-002), degraded threshold=%.2f", qualityScoreThreshold())
	} else {
		log.Println("aeon-modelgw: AEON_PG_DSN not set — FinOps costs computed per-call but not durably recorded, and quality-aware routing (MDL-002) disabled")
	}

	mux := http.NewServeMux()
	// MDL-017: the registry the cost ceiling is read from. Optional like the ledger, and the log says
	// which of the three states this deployment is in — enforcing, or not, and why — because a gateway
	// that has quietly stopped capping spend is the worst of the three.
	if agents != nil && ledger != nil {
		log.Println("aeon-modelgw: cost ceiling live (spec.runtime.budgets from the agent registry; a call naming a run and an agent is capped)")
		log.Printf("aeon-modelgw: /v1/chat/completions is governed too (VRT-AEON-003): send %s and %s to be "+
			"capped and attributed, and %s to be charged once across retries; a call that sends none still "+
			"works and says so in its response",
			api.RunIDHeader, api.AgentManifestRefHeader, api.IdempotencyKeyHeader)
	} else {
		log.Println("aeon-modelgw: cost ceiling NOT enforced (needs AEON_PG_DSN for both the agent registry and the cost ledger)")
	}
	governance := &api.ModelGatewayHandlers{Gateway: gw, Pricing: pricing, Ledger: ledger, Agents: agents}
	governance.Register(mux)
	// VRT-AEON-003 A-3: the SAME handler instance, not a second one configured alike. The defect this
	// closes is that /v1/chat/completions had Aeon's routing and none of its governance — a deployment
	// with a governed door and an ungoverned one, where the ungoverned one is the one documented as
	// needing no code change. Sharing the instance is what makes it impossible for the two to drift.
	(&api.OpenAICompatibleHandlers{Gateway: gw, Bundle: bundle, Governance: governance}).Register(mux)
	if ledger != nil {
		(&api.FinOpsHandlers{Ledger: ledger}).Register(mux)
	}
	if qualityScores != nil {
		(&api.QualityScoreHandlers{Scores: qualityScores}).Register(mux)
	}

	// SEC-005: every route but /healthz and /readyz is behind this. Fatal when unconfigured — see
	// auth.MustLoadFromEnv for why a warning would be worse than not starting.
	callers := auth.MustLoadFromEnv("aeon-modelgw")
	srv := httpserver.New("aeon-modelgw", mux, callers)
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
//
// SEC-006: every credential below comes from secretref.Take, so each one can arrive as
// <NAME>_FILE instead of <NAME> — and either way it is removed from this process's environment once
// read. The endpoints and client IDs beside them stay plain os.Getenv on purpose: they are not
// secrets, scrubbing them would hide them from `docker inspect` where an operator wants to see them,
// and a gateway URL in a child process's environment is a configuration detail and not a disclosure.
func registerProvidersFromEnv(gw *modelgateway.Gateway, bundle modelgateway.ModelPolicyBundleDoc) []string {
	var registered []string

	if key, ok := takeSecret("ANTHROPIC_API_KEY"); ok {
		gw.RegisterProvider(anthropic.Name, &anthropic.Adapter{APIKey: key})
		registered = append(registered, anthropic.Name)
	}
	if key, ok := takeSecret("OPENAI_API_KEY"); ok {
		gw.RegisterProvider(openai.Name, &openai.Adapter{APIKey: key})
		registered = append(registered, openai.Name)
	}
	if key, ok := takeSecret("GOOGLE_API_KEY"); ok {
		gw.RegisterProvider(gemini.Name, &gemini.Adapter{APIKey: key})
		registered = append(registered, gemini.Name)
	}
	if clientID := os.Getenv("PROMETHEUS_CLIENT_ID"); clientID != "" {
		clientSecret, _ := takeSecret("PROMETHEUS_CLIENT_SECRET")
		gw.RegisterProvider(prometheusinference.Name, prometheusinference.New(
			os.Getenv("PROMETHEUS_AUTH_URL"),
			os.Getenv("PROMETHEUS_GATEWAY_URL"),
			clientID,
			clientSecret,
			prometheusScope(bundle),
			os.Getenv("PROMETHEUS_DEFAULT_MODEL"),
		))
		registered = append(registered, prometheusinference.Name)
	}
	if baseURL := os.Getenv("OPENAI_COMPATIBLE_BASE_URL"); baseURL != "" {
		key, _ := takeSecret("OPENAI_COMPATIBLE_API_KEY")
		gw.RegisterProvider(openaicompatible.Name, &openaicompatible.Adapter{
			BaseURL: baseURL,
			APIKey:  key,
		})
		registered = append(registered, openaicompatible.Name)
	}

	return registered
}

// takeSecret resolves one credential and refuses to start the gateway if it is configured wrong —
// two sources at once, or a <NAME>_FILE that cannot be read. NOT configured is not wrong: it returns
// false and the caller skips that adapter, which is how a deployment with no Anthropic account runs.
//
// Fatal rather than a warning, because the alternative was measured in MDL-015's neighbourhood: an
// adapter that silently fails to register does not break anything visibly. Routing falls through to
// the next candidate in the bundle, the run succeeds, and it answers from a different model at a
// different price. A credential whose secret store did not mount has to look like a broken
// deployment, not like a cheaper one.
func takeSecret(name string) (string, bool) {
	value, source, err := secretref.Take(name)
	if err != nil {
		log.Fatalf("aeon-modelgw: %v", err)
	}
	if source == secretref.SourceFile {
		// The source, never the value. Worth a line: "is this deployment on the file path?" is the
		// first question after wiring a secret store, and the honest answer is in the process that
		// read it rather than in the compose file someone believes is in effect.
		log.Printf("aeon-modelgw: %s resolved from %s%s (never entered this process's environment)", name, name, secretref.FileSuffix)
	}
	return value, source != secretref.SourceAbsent
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

// verifyDeclaredModalities is MDL-013: checks the bundle's declarations against the provider's own
// catalog, once at startup.
//
// Once and not per call, because the catalog is nearly free — measured on the real deployment: 2030
// bytes, about a millisecond, and NO authentication. And at startup rather than lazily, because a
// contradiction is a configuration error and the moment to learn about a configuration error is when
// the configuration is loaded.
//
// THE PART THAT MATTERS IS WHAT HAPPENS WHEN THE CATALOG CANNOT BE READ, and it is three states, not
// two:
//
//   - verified and consistent   -> nothing to say
//   - verified and CONTRADICTED -> say so loudly, per contradiction
//   - NOT VERIFIED              -> say THAT, and start anyway
//
// The third is why this does not abort. Refusing to start when the platform is briefly unreachable
// would let a transient blip take down a gateway that can still route to every other provider — worse
// than the problem MDL-013 solves. And silently skipping would be the other failure, the guard that
// passes for the wrong reason, which this session has already been caught by three times. So the
// absence of verification is itself reported.
//
// It does not remove candidates today, deliberately: a contradiction is reported, not enforced. Making
// it fatal is a separate decision about an operator's deployment, and taking it inside a logging
// function would be deciding it by accident.
func verifyDeclaredModalities(bundle modelgateway.ModelPolicyBundleDoc) {
	clientID := os.Getenv("PROMETHEUS_CLIENT_ID")
	if clientID == "" {
		return // no Prometheus candidates can be in play; nothing to verify against
	}
	// SEC-006: the same memoised read registerProvidersFromEnv already did. secretref.Take is
	// destructive, so this second read gets the cached value rather than the empty string the
	// environment now holds — the reason that memoisation exists is exactly this call site.
	clientSecret, _ := takeSecret("PROMETHEUS_CLIENT_SECRET")
	client := &prometheusinference.Client{
		GatewayURL:   os.Getenv("PROMETHEUS_GATEWAY_URL"),
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scope:        prometheusScope(bundle),
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	models, err := client.ListModels(ctx)
	if err != nil {
		log.Printf("aeon-modelgw: MDL-013 modality verification DID NOT RUN (%v) — every prometheus_inference "+
			"declaration in the bundle is unverified, which is not the same as verified-correct", err)
		return
	}

	catalog := make([]modelgateway.CatalogEntry, 0, len(models))
	for _, m := range models {
		catalog = append(catalog, modelgateway.CatalogEntry{Model: m.ID, Modality: m.Modality})
	}

	contradictions := modelgateway.VerifyModalitiesAgainstCatalog(bundle, prometheusinference.Name, catalog)
	if len(contradictions) == 0 {
		log.Printf("aeon-modelgw: MDL-013 modality verification passed against a catalog of %d model(s)", len(catalog))
		return
	}
	for _, c := range contradictions {
		log.Printf("aeon-modelgw: MDL-013 CONTRADICTION: %s", c.Error())
	}
	log.Printf("aeon-modelgw: %d bundle declaration(s) contradict the provider's catalog — routing to them will fail at the first call, with an error from the provider about a model this bundle asserted was fine", len(contradictions))
}
