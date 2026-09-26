package searxng_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/toolexec"
	"github.com/aeon-ai/aeon/go/internal/websearch/searxng"
)

// TestSearchWebReturnsRealResults is TOOL-007's acceptance test.
//
// Before this, search.web was registered in toolexec.NewExecutor as a function that echoed its own
// arguments: {"status": "executed", "tool": "search.web", "args": {...}}. A deep-research run could
// call it, read a successful answer, retrieve nothing, and report success — and every framework
// adapter routes through that same executor, so none of them searched either. TOOL-006 fixed the
// same shape for search.rag and this test's docstring named search.web as the one still lying.
//
// The first subtest runs against a REAL SearXNG instance reaching the REAL internet. Not a recorded
// fixture: the property is that a query returns pages that exist and can be cited, and a fixture
// would assert the shape of a response we wrote ourselves. The remaining subtests use fake upstreams
// because they need failure modes a live instance will not produce on demand.
func TestSearchWebReturnsRealResults(t *testing.T) {
	ctx := context.Background()

	t.Run("a real query returns real, citable pages from the live internet", func(t *testing.T) {
		endpoint := os.Getenv("AEON_TEST_SEARXNG_URL")
		if endpoint == "" {
			t.Skip("AEON_TEST_SEARXNG_URL not set — see make test-go-integration")
		}
		searcher := searxng.New(endpoint)

		results, err := searcher.Search(ctx, "temporal workflow durable execution", 5)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(results) == 0 {
			// Not a skip. An empty answer here is the exact failure this feature removes: the agent
			// would conclude the web has nothing on the subject and reason from that.
			t.Fatal("no results for a query with abundant real answers — the searcher is not working")
		}
		if len(results) > 5 {
			t.Errorf("got %d results, want at most the requested 5", len(results))
		}

		for i, r := range results {
			// The URL is the feature, not a field. A hit an agent cannot point at becomes a claim it
			// presents as sourced, and DR-005's Citation Verifier has nothing to check.
			if !strings.HasPrefix(r.URL, "http://") && !strings.HasPrefix(r.URL, "https://") {
				t.Errorf("result %d has an unusable URL %q — a passage that cannot be located cannot be cited", i, r.URL)
			}
			if r.Title == "" {
				t.Errorf("result %d has no title", i)
			}
		}
		t.Logf("top hit: %q — %s (engine %q)", results[0].Title, results[0].URL, results[0].Engine)
	})

	t.Run("every engine failing is an error, not an empty web", func(t *testing.T) {
		// Measured against the live instance: unresponsive_engines really does arrive as
		// [["duckduckgo","CAPTCHA"]] while other engines answer. This is the case where they ALL
		// refuse — no results, and the reason known. Returning [] here would tell the agent the
		// subject has no coverage anywhere.
		srv := fakeSearx(t, map[string]any{
			"query":                "algo",
			"results":              []any{},
			"unresponsive_engines": []any{[]any{"duckduckgo", "CAPTCHA"}, []any{"google", "timeout"}},
		})

		_, err := searxng.New(srv.URL).Search(ctx, "algo", 5)
		if err == nil {
			t.Fatal("an empty result set with every engine down was returned as an empty answer")
		}
		if !strings.Contains(err.Error(), "CAPTCHA") || !strings.Contains(err.Error(), "timeout") {
			t.Errorf("error = %v, want it to name which engines failed and why", err)
		}
	})

	t.Run("a partially degraded search still answers", func(t *testing.T) {
		// The normal state of a metasearch instance, and it must NOT be an error: the live instance
		// has DuckDuckGo on CAPTCHA continuously while Google answers 36 results.
		srv := fakeSearx(t, map[string]any{
			"query": "algo",
			"results": []any{
				map[string]any{"title": "Real page", "url": "https://example.org/a", "content": "snippet", "engine": "google"},
			},
			"unresponsive_engines": []any{[]any{"duckduckgo", "CAPTCHA"}},
		})

		results, err := searxng.New(srv.URL).Search(ctx, "algo", 5)
		if err != nil {
			t.Fatalf("a search that returned results failed because one engine was down: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("got %d results, want 1", len(results))
		}
	})

	t.Run("a genuinely empty result set with no failures is an answer", func(t *testing.T) {
		// The other half of the distinction. If "empty" were always an error we would have traded one
		// merged fact for another, and a query that truly matches nothing could never be reported.
		srv := fakeSearx(t, map[string]any{"query": "x", "results": []any{}, "unresponsive_engines": []any{}})

		results, err := searxng.New(srv.URL).Search(ctx, "x", 5)
		if err != nil {
			t.Fatalf("an honestly empty search was reported as a failure: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("got %d results, want 0", len(results))
		}
	})

	t.Run("a hit with no URL is dropped rather than returned unciteable", func(t *testing.T) {
		srv := fakeSearx(t, map[string]any{
			"query": "algo",
			"results": []any{
				map[string]any{"title": "No URL", "content": "text", "engine": "e"},
				map[string]any{"title": "Has URL", "url": "https://example.org/b", "content": "text", "engine": "e"},
			},
		})

		results, err := searxng.New(srv.URL).Search(ctx, "algo", 5)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(results) != 1 || results[0].URL != "https://example.org/b" {
			t.Fatalf("results = %+v, want only the hit that can be cited", results)
		}
	})

	t.Run("the JSON API being off is named, not returned as no results", func(t *testing.T) {
		// SearXNG serves HTML by default and answers 403 to a JSON request unless `json` is listed
		// under search.formats. Reported by name because the alternative — an empty result set — is
		// a configuration mistake wearing the shape of a fact about the world.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		t.Cleanup(srv.Close)

		_, err := searxng.New(srv.URL).Search(ctx, "algo", 5)
		if err == nil {
			t.Fatal("a 403 was not reported")
		}
		if !strings.Contains(err.Error(), "search.formats") {
			t.Errorf("error = %v, want it to say how to fix the instance's settings.yml", err)
		}
	})

	t.Run("the tool records which provider served it and whether it left the network", func(t *testing.T) {
		// Both are facts about where a run's evidence came from. Neither can be reconstructed months
		// later from configuration that has changed since.
		srv := fakeSearx(t, map[string]any{
			"query": "algo",
			"results": []any{
				map[string]any{"title": "T", "url": "https://example.org/c", "content": "s", "engine": "google"},
			},
		})
		executor := toolexec.NewExecutor()

		// Before registration the tool does not exist at all — the property that replaced the stub.
		if _, err := executor.Execute("search.web", map[string]any{"query": "algo"}); err == nil {
			t.Fatal("search.web ran without a configured provider — the stub is still there")
		}

		toolexec.RegisterWebSearchTool(executor, searxng.New(srv.URL))
		out, err := executor.Execute("search.web", map[string]any{"query": "algo"})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if out["provider"] != "searxng" {
			t.Errorf("provider = %v, want searxng", out["provider"])
		}
		if out["in_network"] != true {
			t.Errorf("in_network = %v, want true for a self-hosted instance", out["in_network"])
		}
		hits, _ := out["results"].([]map[string]any)
		if len(hits) != 1 || hits[0]["url"] != "https://example.org/c" {
			t.Errorf("results = %+v, want the real hit with its URL", out["results"])
		}
	})

	t.Run("a call with no query is refused instead of searching for nothing", func(t *testing.T) {
		executor := toolexec.NewExecutor()
		toolexec.RegisterWebSearchTool(executor, searxng.New("http://unused.invalid"))

		if _, err := executor.Execute("search.web", map[string]any{}); err == nil {
			t.Fatal("a call with no query was accepted")
		}
	})
}

// fakeSearx serves one canned SearXNG JSON body. Used only for the failure modes a live instance
// will not produce on demand — the end-to-end proof above runs against the real one.
func fakeSearx(t *testing.T, body map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("format"); got != "json" {
			t.Errorf("adapter requested format=%q, want json", got)
		}
		if r.URL.Query().Get("q") == "" {
			t.Error("adapter sent no query")
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			panic(fmt.Sprintf("encoding fake searx body: %v", err))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}
