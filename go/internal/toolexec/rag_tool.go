package toolexec

import (
	"context"
	"fmt"

	"github.com/aeon-ai/aeon/go/internal/rag"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// defaultTopK is deliberately small. A retrieval tool that returns twenty passages pushes the
// filtering job onto the model's context window, which is the expensive place to do it.
const defaultTopK = 5

// RegisterRagTool wires TOOL-006's search.rag: real retrieval over indexed documents.
//
// Every passage carries its locator, and that is the feature, not a detail. A retrieval tool that
// returns text alone lets an agent quote a document it cannot point at, and DR-005's Citation
// Verifier has nothing to check the quote against — the citation becomes an assertion that happens
// to look sourced.
func RegisterRagTool(e *Executor, s *store.RagStore, embedder rag.Embedder, corpus string) {
	e.Register("search.rag", func(args map[string]any) (map[string]any, error) {
		query, _ := args["query"].(string)
		if query == "" {
			return nil, fmt.Errorf("toolexec: search.rag: missing required string arg %q", "query")
		}
		topK := defaultTopK
		if raw, ok := args["top_k"].(float64); ok && raw > 0 {
			topK = int(raw)
		}

		passages, err := rag.Search(context.Background(), s, embedder, corpus, query, topK)
		if err != nil {
			return nil, fmt.Errorf("toolexec: search.rag: %w", err)
		}

		results := make([]map[string]any, 0, len(passages))
		for _, p := range passages {
			results = append(results, map[string]any{
				"locator":     p.Locator,
				"source_path": p.SourcePath,
				"content":     p.Content,
				"score":       p.Score,
			})
		}
		return map[string]any{
			"status":          "executed",
			"tool":            "search.rag",
			"corpus":          corpus,
			"embedding_model": embedder.Model(),
			"passages":        results,
		}, nil
	})
}
