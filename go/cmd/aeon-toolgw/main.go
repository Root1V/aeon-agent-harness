// Command aeon-toolgw is the Tool Gateway (TOOL-001): executes typed tool calls after the Policy
// Engine has checked them (post-argument-generation, pre-execution — see docs/adr/0001), enforces
// idempotency via a dedupe table keyed on idempotency_key (RUN-004), and hosts the MCP
// client/server adapters (TOOL-002/INT-003).
//
// STATUS: the Cedar Policy Engine (SEC-001, docs/adr/0002) is real and wired to a policy-checked
// /execute endpoint — see go/internal/policy and go/internal/api. INT-003's outbound MCP server
// (real tools from the Postgres-backed Tool Registry, TOOL-001, exposed at /mcp) is real too. The
// idempotency dedupe table (RUN-004) is not yet implemented — see roadmap.md, still `TODO`.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aeon-ai/aeon/go/internal/api"
	"github.com/aeon-ai/aeon/go/internal/httpserver"
	aeonmcp "github.com/aeon-ai/aeon/go/internal/mcp"
	"github.com/aeon-ai/aeon/go/internal/policy"
	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
	"github.com/aeon-ai/aeon/go/internal/secrets"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
	"github.com/aeon-ai/aeon/go/internal/tracing"
)

func main() {
	if p := os.Getenv("AEON_TOOLGW_PORT"); p != "" {
		os.Setenv("AEON_PORT", p)
	} else {
		os.Setenv("AEON_PORT", "9403")
	}

	otelEndpoint := os.Getenv("AEON_OTEL_ENDPOINT")
	if otelEndpoint == "" {
		otelEndpoint = "otel-collector:4318"
	}
	if _, shutdown, err := tracing.Init(context.Background(), "aeon-toolgw", otelEndpoint); err != nil {
		log.Printf("aeon-toolgw: tracing disabled: %v", err)
	} else {
		defer shutdown(context.Background())
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

	executor := toolexec.NewExecutor()

	// SEC-002: a real Secret Broker — callers get short-lived, opaque lease references (POST
	// /secrets/issue), never the raw values; only "secrets.whoami"'s own server-side execution ever
	// resolves one (go/internal/secrets, go/internal/toolexec/secrets_tool.go). AEON_SECRET_NAMES is
	// a comma-separated list of secret names to load from AEON_SECRET_<NAME> env vars — optional,
	// like AEON_PG_DSN above: with none configured, Issue simply fails per-name rather than this
	// process refusing to start.
	var secretNames []string
	if raw := os.Getenv("AEON_SECRET_NAMES"); raw != "" {
		for _, name := range strings.Split(raw, ",") {
			if name = strings.TrimSpace(name); name != "" {
				secretNames = append(secretNames, name)
			}
		}
	}
	broker := secrets.NewBrokerFromEnv(secretNames)
	toolexec.RegisterSecretsTool(executor, broker)

	mux := http.NewServeMux()
	handlers := &api.ToolGatewayHandlers{
		Policy:   engine,
		Executor: executor,
	}
	handlers.Register(mux)
	(&api.SecretBrokerHandlers{Broker: broker}).Register(mux)

	// INT-003: expose the real, Postgres-backed Tool Registry (TOOL-001) as a real outbound MCP
	// server, so any real MCP client (LangGraph, CrewAI, Claude Code, ...) can list and call
	// Aeon's governed catalog — every call still goes through the same Cedar policy check as a
	// native /execute call (see go/internal/mcp's NewToolGatewayServer). Optional: without
	// AEON_PG_DSN, /mcp is simply not mounted — /check-policy and /execute still work.
	if dsn := os.Getenv("AEON_PG_DSN"); dsn != "" {
		s, err := store.Connect(context.Background(), dsn)
		if err != nil {
			log.Fatalf("aeon-toolgw: connecting to Postgres for the Tool Registry: %v", err)
		}
		defer s.Close()

		// TOOL-005: the execution dedupe table. Without it a request carrying an idempotency_key is
		// refused rather than executed — a caller that asked for protection must not silently get an
		// effect instead.
		handlers.Executions = s.ToolExecutions()
		log.Println("aeon-toolgw: execution deduplication live (idempotency_key honoured on /execute)")

		// TOOL-006: search.rag over indexed documents. Registered only when an embedding model is
		// named AND the provider it needs is configured — an unregistered tool is denied with a
		// clear "unknown tool" rather than answering from an empty corpus, which would look like a
		// document that says nothing.
		if embedder := ragEmbedderFromEnv(); embedder != nil {
			corpus := os.Getenv("AEON_RAG_CORPUS")
			if corpus == "" {
				corpus = "default"
			}
			toolexec.RegisterRagTool(executor, s.RagStore(), embedder, corpus)
			log.Printf("aeon-toolgw: search.rag live over corpus %q using embedding model %q", corpus, embedder.Model())
		} else {
			log.Println("aeon-toolgw: search.rag not registered (set AEON_EMBEDDING_MODEL and the Prometheus credentials)")
		}

		tools, err := s.ToolRegistry().List(context.Background())
		if err != nil {
			log.Fatalf("aeon-toolgw: listing the Tool Registry for the MCP server: %v", err)
		}
		mcpServer := aeonmcp.NewToolGatewayServer(tools, engine, executor)
		mux.Handle("/mcp", sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return mcpServer }, &sdkmcp.StreamableHTTPOptions{Stateless: true}))
		log.Printf("aeon-toolgw: MCP server live at /mcp with %d tool(s) from the Tool Registry", len(tools))
	} else {
		log.Println("aeon-toolgw: AEON_PG_DSN not set — /mcp not mounted (Tool Registry unavailable)")
	}

	srv := httpserver.New("aeon-toolgw", mux)
	log.Println("aeon-toolgw starting (policy-checked execution + outbound MCP server live; dedupe table not yet implemented — see roadmap.md RUN-004)")
	httpserver.MustListenAndServe(srv)
}

// ragEmbedderFromEnv builds TOOL-006's embedder, or nil when this deployment has not configured one.
//
// Returning nil rather than a stub is deliberate: a stub embedder would let search.rag answer with
// passages retrieved from a meaningless vector space, and the agent would cite them. A tool that is
// not there fails loudly at the first call.
func ragEmbedderFromEnv() *prometheusinference.Embedder {
	model := os.Getenv("AEON_EMBEDDING_MODEL")
	authURL := os.Getenv("PROMETHEUS_AUTH_URL")
	gatewayURL := os.Getenv("PROMETHEUS_GATEWAY_URL")
	clientID := os.Getenv("PROMETHEUS_CLIENT_ID")
	clientSecret := os.Getenv("PROMETHEUS_CLIENT_SECRET")
	if model == "" || authURL == "" || gatewayURL == "" || clientID == "" || clientSecret == "" {
		return nil
	}
	return &prometheusinference.Embedder{
		Client: &prometheusinference.Client{
			GatewayURL: gatewayURL,
			Tokens: &prometheusinference.TokenSource{
				AuthURL: authURL, ClientID: clientID, ClientSecret: clientSecret,
				// The token must carry model:<id> for the embedding model specifically. Being
				// authorised for it is not enough — .env.example records that confirmed live.
				Scope: "inference:read model:" + model,
			},
		},
		ModelID: model,
	}
}
