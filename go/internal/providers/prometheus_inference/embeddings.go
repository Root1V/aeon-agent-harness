package prometheusinference

import (
	"context"
	"fmt"
	"net/http"
)

// Embedder implements rag.Embedder against Prometheus's /v1/embeddings (TOOL-006).
//
// It lives here, with the provider that knows how to talk to this platform, rather than in the rag
// package — which names no provider at all. That split is MDL-017's rule applied to a second place:
// the harness knows that text goes in and vectors come out, and a deployment supplies the rest.
type Embedder struct {
	Client *Client
	// ModelID must be a model whose modality is "embedding" in the catalog AND one this client's
	// token carries a model:<id> scope for. Being authorised is not enough — a token requested
	// without the scope comes back valid and sees zero models (see .env.example, confirmed live).
	ModelID string
}

// Model reports which model produced the vectors, so the store can record it per chunk and refuse a
// later search with a different one.
func (e *Embedder) Model() string { return e.ModelID }

type embeddingsRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
}

// Embed returns one vector per input, in input order.
//
// The order check is not paranoia about this gateway: the response carries an explicit `index` per
// object precisely because the API does not promise order, and a silently mis-ordered batch would
// attach every passage's vector to its neighbour — retrieval would still work, and return the wrong
// passages with high confidence.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	var parsed embeddingsResponse
	if err := e.Client.doAuthenticatedJSON(ctx, http.MethodPost, "/v1/embeddings",
		embeddingsRequest{Model: e.ModelID, Input: texts}, &parsed); err != nil {
		return nil, fmt.Errorf("prometheus_inference: embedding %d text(s) with %s: %w", len(texts), e.ModelID, err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("prometheus_inference: asked for %d embeddings, got %d", len(texts), len(parsed.Data))
	}

	out := make([][]float32, len(texts))
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= len(out) {
			return nil, fmt.Errorf("prometheus_inference: embedding index %d is outside the batch", item.Index)
		}
		if out[item.Index] != nil {
			return nil, fmt.Errorf("prometheus_inference: embedding index %d returned twice", item.Index)
		}
		out[item.Index] = item.Embedding
	}
	for i, v := range out {
		if len(v) == 0 {
			return nil, fmt.Errorf("prometheus_inference: no embedding returned for input %d", i)
		}
	}
	return out, nil
}
