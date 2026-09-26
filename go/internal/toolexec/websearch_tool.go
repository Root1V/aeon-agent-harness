package toolexec

import (
	"context"
	"fmt"

	"github.com/aeon-ai/aeon/go/internal/websearch"
)

// defaultWebSearchTopK matches search.rag's defaultTopK, and for the same reason: a tool that
// returns twenty hits moves the filtering into the model's context window, which is the expensive
// place to do it.
const defaultWebSearchTopK = 5

// RegisterWebSearchTool wires TOOL-007's search.web: real results from a real search provider.
//
// Until this, search.web echoed its own arguments back — `{"status": "executed", "args": {...}}` —
// so a deep-research run could call it, receive a successful-looking answer, and retrieve nothing.
// Every framework adapter routes through this executor, so none of them searched either.
//
// The answer records which provider served it and whether that provider is inside the deployment's
// network. Both are facts about where a run's evidence came from, and neither can be reconstructed
// later from configuration that has since changed.
func RegisterWebSearchTool(e *Executor, searcher websearch.Searcher) {
	e.Register("search.web", func(args map[string]any) (map[string]any, error) {
		query, _ := args["query"].(string)
		if query == "" {
			return nil, fmt.Errorf("toolexec: search.web: missing required string arg %q", "query")
		}
		topK := defaultWebSearchTopK
		if raw, ok := args["top_k"].(float64); ok && raw > 0 {
			topK = int(raw)
		}

		results, err := searcher.Search(context.Background(), query, topK)
		if err != nil {
			return nil, fmt.Errorf("toolexec: search.web: %w", err)
		}

		hits := make([]map[string]any, 0, len(results))
		for _, r := range results {
			hits = append(hits, map[string]any{
				"title":   r.Title,
				"url":     r.URL,
				"snippet": r.Snippet,
				"engine":  r.Engine,
			})
		}
		return map[string]any{
			"status":     "executed",
			"tool":       "search.web",
			"provider":   searcher.Name(),
			"in_network": searcher.InNetwork(),
			"query":      query,
			"results":    hits,
		}, nil
	})
}
