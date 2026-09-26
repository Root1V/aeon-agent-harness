// Package searxng is TOOL-007's default Searcher: a self-hosted SearXNG instance.
//
// Chosen because it is the only option that costs a deployment nothing and sends nothing to a third
// party. Brave withdrew its free tier in early 2026 (metered billing, card required, no spending
// cap), and every commercial alternative puts the query — which the model writes from the run's
// context — into someone else's logs. SearXNG runs as a container in deploy/compose, so the query
// never leaves the deployment; the engines it federates see only the query, never the run.
//
// What this costs in exchange, said plainly: a metasearch instance depends on upstream engines that
// rate-limit, so it is less predictable than a paid API. That is a real trade and it is the reason
// Searcher is an interface.
package searxng

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aeon-ai/aeon/go/internal/websearch"
)

// Name is the provider name recorded in the tool's answer and in traces.
const Name = "searxng"

// defaultTimeout bounds one search. A metasearch instance waits on several upstream engines, so it
// is slower than a single API; 20s is generous enough for a cold instance and short enough that a
// hung engine does not hold a run's step open indefinitely.
const defaultTimeout = 20 * time.Second

// Searcher queries a SearXNG instance's JSON API.
type Searcher struct {
	// BaseURL is the instance root, e.g. http://searxng:8080.
	BaseURL string
	// HTTPClient is optional; a default with defaultTimeout is used when nil.
	HTTPClient *http.Client
	// Declared is whether this instance is inside the deployment's network. It defaults to true
	// because that is what deploy/compose provides, but it is a field rather than a constant: the
	// same adapter pointed at a public SearXNG instance is a third party, and nothing about the
	// code would change. See websearch.Searcher.InNetwork.
	Declared *bool
}

// New returns a Searcher for a self-hosted instance, declared in-network.
func New(baseURL string) *Searcher {
	inNetwork := true
	return &Searcher{BaseURL: strings.TrimRight(baseURL, "/"), Declared: &inNetwork}
}

// Name identifies the provider.
func (s *Searcher) Name() string { return Name }

// InNetwork reports the declaration, defaulting to true for a self-hosted instance.
func (s *Searcher) InNetwork() bool { return s.Declared == nil || *s.Declared }

// searxResponse is the subset of SearXNG's JSON answer this adapter reads.
//
// Deliberately absent: number_of_results. The live instance returns null for it, so a caller that
// trusted it would read "no results" off a field the provider does not populate.
type searxResponse struct {
	Query   string `json:"query"`
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
		Engine  string `json:"engine"`
	} `json:"results"`
	// UnresponsiveEngines is [[engine, reason], ...] — measured against the live instance as
	// [["duckduckgo", "CAPTCHA"]]. Typed as [][]any rather than [][]string because a shape change
	// upstream would otherwise fail the whole decode over a field that is only diagnostic.
	UnresponsiveEngines [][]any `json:"unresponsive_engines"`
}

// describeFailures renders unresponsive_engines for an error message.
func describeFailures(raw [][]any) string {
	parts := make([]string, 0, len(raw))
	for _, pair := range raw {
		switch len(pair) {
		case 0:
			continue
		case 1:
			parts = append(parts, fmt.Sprint(pair[0]))
		default:
			parts = append(parts, fmt.Sprintf("%v (%v)", pair[0], pair[1]))
		}
	}
	return strings.Join(parts, ", ")
}

// Search runs one query against the instance's JSON API.
func (s *Searcher) Search(ctx context.Context, query string, limit int) ([]websearch.Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("searxng: empty query")
	}
	if s.BaseURL == "" {
		return nil, fmt.Errorf("searxng: no BaseURL configured")
	}
	if limit <= 0 {
		limit = 5
	}

	endpoint := s.BaseURL + "/search?" + url.Values{
		"q":      {query},
		"format": {"json"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("searxng: building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searxng: querying %s: %w", s.BaseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 403 is the specific failure worth naming: SearXNG serves HTML by default and returns it
		// for a JSON request unless `json` is listed under search.formats. A deployment that skipped
		// that config gets a refusal here rather than an empty result set, because an empty result
		// set reads as "the web has nothing" (see deploy/compose/searxng/settings.yml).
		if resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("searxng: %s refused the JSON API (403) — add `json` to search.formats in its settings.yml", s.BaseURL)
		}
		return nil, fmt.Errorf("searxng: %s returned %d", s.BaseURL, resp.StatusCode)
	}

	var body searxResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("searxng: decoding response: %w", err)
	}

	// An empty result set means two different things and they must not arrive as the same answer.
	// With every engine refusing — CAPTCHA, rate limit, timeout — "no results" is a broken searcher,
	// and returning it as an empty list tells the agent the web has nothing on the subject. It would
	// then reason from that, and report it. Partial failure is NOT an error: one engine on CAPTCHA
	// while another answers is the normal state of a metasearch instance.
	if len(body.Results) == 0 && len(body.UnresponsiveEngines) > 0 {
		return nil, fmt.Errorf("searxng: no results and every engine failed — %s: this is a broken searcher, not an empty web", describeFailures(body.UnresponsiveEngines))
	}

	out := make([]websearch.Result, 0, limit)
	for _, r := range body.Results {
		// A hit with no URL is dropped rather than returned without one. An agent that receives a
		// title and a snippet with nowhere to point will still cite it.
		if r.URL == "" {
			continue
		}
		out = append(out, websearch.Result{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Content,
			Engine:  r.Engine,
		})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
