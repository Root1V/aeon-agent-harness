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
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aeon-ai/aeon/go/internal/api"
	"github.com/aeon-ai/aeon/go/internal/auth"
	"github.com/aeon-ai/aeon/go/internal/httpserver"
	aeonmcp "github.com/aeon-ai/aeon/go/internal/mcp"
	"github.com/aeon-ai/aeon/go/internal/policy"
	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
	"github.com/aeon-ai/aeon/go/internal/secretref"
	"github.com/aeon-ai/aeon/go/internal/secrets"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
	"github.com/aeon-ai/aeon/go/internal/tracing"
	"github.com/aeon-ai/aeon/go/internal/websearch/searxng"
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
	if _, shutdown, err := tracing.Init(context.Background(), "toolgw", "api", otelEndpoint); err != nil {
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

	// TOOL-007: search.web over a real search provider. Registered only when one is configured, so a
	// deployment without search gets "unknown tool" on the first call instead of a successful-looking
	// answer with no results — which is what this replaced.
	if endpoint := os.Getenv("AEON_WEBSEARCH_SEARXNG_URL"); endpoint != "" {
		searcher := searxng.New(endpoint)
		toolexec.RegisterWebSearchTool(executor, searcher)
		log.Printf("aeon-toolgw: search.web live via %s at %s (in-network: %t)", searcher.Name(), endpoint, searcher.InNetwork())
	} else {
		log.Println("aeon-toolgw: search.web not registered (set AEON_WEBSEARCH_SEARXNG_URL)")
	}

	// TOOL-008: repository.read over a real directory. Registered only when a root is configured, for
	// the same reason as search.web above — and this tool is the reason that rule had a hole: it was in
	// the policy bundle and in the agent manifest's tools.allow with NOTHING behind it, so the manifest
	// declared a tool this gateway answered "unknown tool" for. CI-001 found it by counting skips.
	//
	// The root is opened ONCE, here, and the handle is what the tool uses — so containment is decided at
	// startup by the operating system rather than per call by string comparison. A root that is missing
	// or is not a directory is a configuration error worth failing on: a gateway that advertises
	// repository.read and answers every call with an error is the "present but lying" state TOOL-007
	// removed.
	if repoRoot := os.Getenv("AEON_REPOSITORY_ROOT"); repoRoot != "" {
		// The root is checked for credentials BEFORE it is opened, and a failure is fatal. This is not
		// defensive programming: the first version of this feature pointed the root at the whole
		// repository and a probe through the real gateway returned `.env` — including
		// PROMETHEUS_CLIENT_SECRET — to a caller holding the public development token. The tool was
		// doing what it was told. Refusing to start is the only answer that does not depend on somebody
		// noticing.
		if err := toolexec.CheckRepositoryRoot(repoRoot); err != nil {
			log.Fatalf("aeon-toolgw: %v", err)
		}
		root, err := os.OpenRoot(repoRoot)
		if err != nil {
			log.Fatalf("aeon-toolgw: AEON_REPOSITORY_ROOT=%s is not an openable directory: %v", repoRoot, err)
		}
		defer root.Close()
		toolexec.RegisterRepositoryReadTool(executor, root, repoRoot)
		log.Printf("aeon-toolgw: repository.read live over %s (read-only, contained by os.Root)", repoRoot)
	} else {
		log.Println("aeon-toolgw: repository.read not registered (set AEON_REPOSITORY_ROOT)")
	}

	// TOOL-009: artifact.read over the artifact store a run writes to. Registered only when a root is
	// configured, like the two above. NOT checked for credentials the way the repository root is: this
	// directory holds what our own runs produced, and a credentials scan of it would be noise rather
	// than a guard — the repository check exists because an operator can point THAT at a source
	// checkout, which is a mistake with a known shape.
	if artifactRoot := os.Getenv("AEON_ARTIFACT_ROOT"); artifactRoot != "" {
		// MkdirAll and not a fatal on absence: the writer is the Python worker and the reader is this
		// process, so on a cold stack the gateway can come up BEFORE any run has produced anything. An
		// empty store is a correct state; a missing directory is not a configuration error.
		if err := os.MkdirAll(artifactRoot, 0o755); err != nil {
			log.Fatalf("aeon-toolgw: AEON_ARTIFACT_ROOT=%s cannot be created: %v", artifactRoot, err)
		}
		root, err := os.OpenRoot(artifactRoot)
		if err != nil {
			log.Fatalf("aeon-toolgw: AEON_ARTIFACT_ROOT=%s is not an openable directory: %v", artifactRoot, err)
		}
		defer root.Close()
		toolexec.RegisterArtifactReadTool(executor, root, artifactRoot)
		log.Printf("aeon-toolgw: artifact.read live over %s (read-only, contained by os.Root)", artifactRoot)
	} else {
		log.Println("aeon-toolgw: artifact.read not registered (set AEON_ARTIFACT_ROOT)")
	}

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
	broker, err := secrets.NewBrokerFromEnv(secretNames)
	if err != nil {
		log.Fatalf("aeon-toolgw: %v", err)
	}
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

		// INT-011: a policy denial is journalled as a known outcome of the step, so a run suspended on a
		// refusal can be resumed instead of looking like a step whose fate nobody knows. Without a DSN the
		// denial still denies — it just answers journalled=false, which is a reported gap and not a silent
		// one.
		handlers.Checkpointer = s.Checkpointer()
		log.Println("aeon-toolgw: policy denials journalled as known outcomes (INT-011)")

		// A2A-002: governed A2A egress as a data-plane proxy. A framework points its A2A client at
		// /a2a/egress/{remote_agent_id} and changes no code; the destination must be declared, Cedar must
		// permit it, the credential is injected here, and the delegation is recorded.
		//
		// The fan-out limit is deliberately OFF unless configured. Width is the harness's half of blast
		// radius (depth is Synaptum's), but picking a number for someone else's deployment would be
		// inventing a bound rather than enforcing one they chose.
		egress := &api.A2AEgressHandlers{
			Policy:             engine,
			RemoteAgents:       s.RemoteAgents(),
			Delegations:        s.A2ADelegations(),
			Broker:             broker,
			MaxInFlightPerRun:  intFromEnv("AEON_A2A_MAX_INFLIGHT_PER_RUN", 0),
			InFlightStaleAfter: time.Duration(intFromEnv("AEON_A2A_INFLIGHT_STALE_SECONDS", 3600)) * time.Second,
		}
		egress.Register(mux)
		if egress.MaxInFlightPerRun > 0 {
			log.Printf("aeon-toolgw: governed A2A egress live at /a2a/egress/{remote_agent_id}, fan-out limit %d per run (A2A-002)", egress.MaxInFlightPerRun)
		} else {
			log.Println("aeon-toolgw: governed A2A egress live at /a2a/egress/{remote_agent_id}, fan-out UNLIMITED (set AEON_A2A_MAX_INFLIGHT_PER_RUN) (A2A-002)")
		}

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
		mcpServer, catalog := aeonmcp.NewToolGatewayCatalog(tools, engine, executor)
		mux.Handle("/mcp", sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return mcpServer }, &sdkmcp.StreamableHTTPOptions{Stateless: true}))
		// The count reported is the DISTINCT TOOLS exposed, not the registry rows read. List returns every
		// version of every tool, so the old message said "213 tool(s)" for a catalogue of a handful — a
		// number that looked like a fact about the MCP surface and was a fact about the table.
		exposed := len(aeonmcp.CurrentVersions(tools))
		log.Printf("aeon-toolgw: MCP server live at /mcp with %d tool(s) exposed from %d Tool Registry row(s)",
			exposed, len(tools))

		// INT-003's live refresh. The registry is written by aeon-controlplane, a different process, so
		// without this a tool registered after start-up stayed invisible until aeon-toolgw was restarted.
		// Clients learn through the protocol's own notifications/tools/list_changed, which the MCP SDK
		// emits from AddTool/RemoveTools — and Catalog.Apply only touches what actually differs, so an
		// unchanged catalogue sends no notification at all.
		refresh := time.Duration(intFromEnv("AEON_MCP_CATALOG_REFRESH_SECONDS", 30)) * time.Second
		if refresh > 0 {
			go catalog.Watch(context.Background(), s.ToolRegistry(), refresh)
			log.Printf("aeon-toolgw: MCP catalog refreshing every %s (AEON_MCP_CATALOG_REFRESH_SECONDS=0 disables)", refresh)
		} else {
			log.Println("aeon-toolgw: MCP catalog refresh DISABLED — a tool registered after now stays invisible until restart")
		}
	} else {
		log.Println("aeon-toolgw: AEON_PG_DSN not set — /mcp not mounted (Tool Registry unavailable)")
	}

	// SEC-005: every route but /healthz and /readyz is behind this. Fatal when unconfigured — see
	// auth.MustLoadFromEnv for why a warning would be worse than not starting.
	callers := auth.MustLoadFromEnv("aeon-toolgw")
	srv := httpserver.New("aeon-toolgw", mux, callers)
	// Deliberately says nothing about which optional subsystems are live: every one of them logs for
	// itself above, from the line that knows whether it actually came up. This line used to claim "dedupe
	// table not yet implemented" directly underneath the line reporting deduplication as live — a summary
	// that restates what the lines above already said is a second copy of the truth, and it is the copy
	// nobody updates.
	log.Println("aeon-toolgw starting (policy-checked execution; see the lines above for which optional subsystems came up)")
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
	// SEC-006: the credential (and only the credential) comes from secretref, so it can arrive as
	// PROMETHEUS_CLIENT_SECRET_FILE and is removed from this process's environment once read. That
	// matters more here than in aeon-modelgw: this is the process that executes tools, and TOOL-003's
	// sandbox and TOOL-008's repository.read are the two surfaces an agent's own output reaches.
	clientSecret, _, err := secretref.Take("PROMETHEUS_CLIENT_SECRET")
	if err != nil {
		log.Fatalf("aeon-toolgw: %v", err)
	}
	if model == "" || authURL == "" || gatewayURL == "" || clientID == "" || clientSecret == "" {
		return nil
	}
	return &prometheusinference.Embedder{
		Client: &prometheusinference.Client{
			// MDL-009: one address. The gateway serves /oauth2/token as well as /v1/, so the
			// separate auth URL is gone — keeping it was the footgun Axonium removed from their
			// own SDK: two addresses to change, and forgetting the second minted tokens against
			// the wrong party without anything failing.
			GatewayURL:   gatewayURL,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			// The token must carry model:<id> for the embedding model specifically. Being
			// authorised for it is not enough — confirmed live.
			Scope: "inference:read model:" + model,
		},
		ModelID: model,
	}
}

// intFromEnv reads a whole number from the environment, falling back when unset or unparseable.
//
// An unparseable value falls back rather than aborting, and it SAYS SO: a typo in a limit should not stop
// a gateway that can still route everything else, but a limit silently read as its default is how an
// operator comes to believe a bound is in force that is not.
func intFromEnv(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("aeon-toolgw: %s=%q is not a number, using %d", name, raw, fallback)
		return fallback
	}
	return n
}
