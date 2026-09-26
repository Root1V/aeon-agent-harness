// Package websearch is TOOL-007's provider seam: real web search, with no provider named here.
//
// The interface exists for the same reason rag.Embedder does. A harness that hard-codes a search
// vendor makes every deployment inherit that vendor's terms, its egress path and its outage; and
// MDL-017 already removed one provider name from the routing core for exactly this reason. What a
// deployment searches with is a deployment's decision.
package websearch

import "context"

// Result is one search hit, reduced to what an agent can act on and cite.
//
// URL is not optional and not a convenience: a result an agent cannot point at is a claim it will
// present as sourced. This is the same reason store.Chunk carries a locator (TOOL-006) — DR-005's
// Citation Verifier needs something to check against, and a title plus a snippet is not it.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	// Engine is which upstream produced this hit, when the provider reports it. A metasearch
	// provider aggregates several, and knowing which one answered is what makes a bad result
	// traceable to a source rather than to "the web".
	Engine string `json:"engine,omitempty"`
}

// Searcher is what a deployment plugs in. Implementations live in subpackages, one per provider.
type Searcher interface {
	// Search returns at most limit results, best first.
	Search(ctx context.Context, query string, limit int) ([]Result, error)

	// Name identifies the provider in traces and in the tool's answer. It is not decoration: when a
	// run's evidence is audited, "which search provider served this" is a question with one right
	// answer, and reconstructing it from configuration months later is not the same as recording it.
	Name() string

	// InNetwork reports whether this provider runs inside the deployment's own network.
	//
	// It is declared rather than inferred, exactly like modelgateway.Candidate.InNetwork: a query is
	// written by the model from the run's context, so it can carry fragments of whatever the run is
	// reading. Sending that to a third party is an egress decision, and a provider that is asked to
	// classify itself by URL shape would get it wrong the first time someone put a proxy in front.
	InNetwork() bool
}
