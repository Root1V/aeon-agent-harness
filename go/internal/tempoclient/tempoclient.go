// Package tempoclient is a minimal real client for Tempo's search API — shared by `aeon trace`
// (go/cmd/aeon) and the Agent Console (go/internal/api/console_handlers.go, OBS-002) so both query
// real trace data the same way, rather than each re-implementing the same HTTP call and JSON shape.
package tempoclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Trace is one trace-level summary row from Tempo's /api/search — the same fields `aeon trace`
// already printed before this package existed.
type Trace struct {
	TraceID         string `json:"traceID"`
	RootServiceName string `json:"rootServiceName"`
	RootTraceName   string `json:"rootTraceName"`
	DurationMs      int    `json:"durationMs"`
}

type searchResponse struct {
	Traces []Trace `json:"traces"`
}

// SearchByAgentName queries baseURL (Tempo's query-frontend, e.g. http://localhost:3200) for every
// trace whose invoke_agent span carries agentName as gen_ai.agent.name — the same attribute OBS-001
// stamps with a run's run_id, so passing a run_id here finds that run's real spans.
func SearchByAgentName(ctx context.Context, baseURL, agentName string) ([]Trace, error) {
	traceQL := fmt.Sprintf(`{ span.gen_ai.agent.name = %q }`, agentName)
	reqURL := baseURL + "/api/search?" + url.Values{"q": {traceQL}, "limit": {"50"}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("tempoclient: building request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tempoclient: querying Tempo at %s: %w", baseURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tempoclient: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tempoclient: tempo at %s returned %d: %s", baseURL, resp.StatusCode, string(body))
	}

	var parsed searchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("tempoclient: decoding response: %w", err)
	}
	return parsed.Traces, nil
}
