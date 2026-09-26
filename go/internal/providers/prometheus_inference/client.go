package prometheusinference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Model mirrors one entry of GET /v1/models and /v1/models/mine's response.
type Model struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	OwnedBy       string `json:"owned_by"`
	ContextLength int    `json:"context_length"`
	Family        string `json:"family"`
	Quantization  string `json:"quantization"`
	Modality      string `json:"modality"`
}

type modelsResponse struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// Client talks to the Prometheus gateway's OpenAI-compatible API.
type Client struct {
	GatewayURL string // e.g. http://127.0.0.1:8020
	Tokens     *TokenSource
	HTTPClient *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// ListModels calls the public GET /v1/models — every active model on the gateway, no token
// required. Does not imply this client has access to all of them; see ListMyModels.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.GatewayURL, "/")+"/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("prometheus_inference: building models request: %w", err)
	}
	var parsed modelsResponse
	if err := c.doJSON(req, &parsed); err != nil {
		return nil, fmt.Errorf("prometheus_inference: listing models: %w", err)
	}
	return parsed.Data, nil
}

// ListMyModels calls the authenticated GET /v1/models/mine — only the models this client's scopes
// actually grant. Useful to check access before an inference call rather than guessing and
// hitting a 403.
func (c *Client) ListMyModels(ctx context.Context) ([]Model, error) {
	var parsed modelsResponse
	if err := c.doAuthenticatedJSON(ctx, http.MethodGet, "/v1/models/mine", nil, &parsed); err != nil {
		return nil, fmt.Errorf("prometheus_inference: listing my models: %w", err)
	}
	return parsed.Data, nil
}

// ChatCompletion calls POST /v1/chat/completions. requestBody follows the OpenAI Chat Completions
// request shape (model, messages[], stream, max_tokens, temperature, tools/tool_choice) — the
// gateway accepts the same shape verbatim, so the caller builds a plain map/struct exactly as it
// would for the real OpenAI API and this method neither adds nor strips fields.
func (c *Client) ChatCompletion(ctx context.Context, requestBody map[string]any) (map[string]any, error) {
	parsed, _, err := c.ChatCompletionWithRequestID(ctx, requestBody)
	return parsed, err
}

// ChatCompletionWithRequestID also returns the platform's own id for this call, taken from the
// `x-request-id` response header (OBS-007).
//
// It is a header and not a body field, and that distinction cost an hour: the body carries
// `id: chatcmpl-...` and the headers carry BOTH `x-trace-id` and `x-request-id`, which are different
// UUIDs. GET /v1/usage/{id} accepts only the last of the three — measured against the live
// deployment, where the other two both answer 404. Without this id there is nothing to reconcile
// against, because the join key between our ledger and theirs simply would not exist.
//
// An empty string is returned rather than an error when the header is absent: a missing id makes the
// call unreconcilable, not failed, and refusing the inference over it would trade a bookkeeping gap
// for an outage.
func (c *Client) ChatCompletionWithRequestID(ctx context.Context, requestBody map[string]any) (map[string]any, string, error) {
	var parsed map[string]any
	header, err := c.doAuthenticatedJSONWithHeader(ctx, http.MethodPost, "/v1/chat/completions", requestBody, &parsed)
	if err != nil {
		return nil, "", fmt.Errorf("prometheus_inference: chat completion: %w", err)
	}
	return parsed, header.Get("x-request-id"), nil
}

// UsageRecord is the platform's own accounting for ONE request, from GET /v1/usage/{request_id}
// (OBS-007). Transcribed from a real answer on 2026-09-26.
//
// Reading this needs no admin scope, which is what makes reconciliation possible at all from a
// normal client — Axonium asked for that endpoint for another purpose entirely.
type UsageRecord struct {
	RequestID   string `json:"request_id"`
	Model       string `json:"model"`
	RequestKind string `json:"request_kind"`
	Usage       struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	TerminationReason string `json:"termination_reason"`
	// CostUSD is a pointer because the platform really does return null for it: eleven usage rows on
	// this deployment have no cost, created when two modalities were added to the registry and not to
	// the price table. Their retariffing script deliberately left them null — "null is the exact
	// record of a period without a tariff, not bad data" — so a client that coerced it to 0 would
	// erase the period in which the platform could not price anything.
	CostUSD *float64 `json:"cost_usd"`
	// Rates are the prices ACTUALLY APPLIED to this request, which is what makes the reconciliation a
	// comparison of two derivations rather than a copy of one number: the cost can be recomputed from
	// these and checked. Measured: (12 prompt x 0.2 + 25 completion x 0.6) / 1e6 = 1.74e-05, exactly
	// the cost_usd returned.
	Rates struct {
		PromptPricePer1M     *float64 `json:"prompt_price_per_1m"`
		CompletionPricePer1M *float64 `json:"completion_price_per_1m"`
		ImagePriceEach       *float64 `json:"image_price_each"`
	} `json:"rates"`
	InstanceID string `json:"instance_id"`
	CreatedAt  string `json:"created_at"`
}

// Usage fetches the platform's accounting for one request.
func (c *Client) Usage(ctx context.Context, requestID string) (*UsageRecord, error) {
	if requestID == "" {
		return nil, fmt.Errorf("prometheus_inference: usage: empty request id")
	}
	var rec UsageRecord
	if err := c.doAuthenticatedJSON(ctx, http.MethodGet, "/v1/usage/"+requestID, nil, &rec); err != nil {
		return nil, fmt.Errorf("prometheus_inference: usage for %s: %w", requestID, err)
	}
	return &rec, nil
}

// doAuthenticatedJSON attaches a bearer token, sends body as JSON (if non-nil), decodes the JSON
// response into out, and retries exactly once — with a freshly forced token — if the first
// attempt comes back 401. A stateless-token platform with a 5-minute minimum TTL means a 401
// almost always just means "expired a little early relative to our clock", not bad credentials.
func (c *Client) doAuthenticatedJSON(ctx context.Context, method, path string, body any, out any) error {
	_, err := c.doAuthenticatedJSONWithHeader(ctx, method, path, body, out)
	return err
}

// doAuthenticatedJSONWithHeader is doAuthenticatedJSON plus the response headers, which OBS-007
// needs because the platform's request id travels there and not in the body.
func (c *Client) doAuthenticatedJSONWithHeader(ctx context.Context, method, path string, body any, out any) (http.Header, error) {
	attempt := func(forceFreshToken bool) (*http.Response, error) {
		if forceFreshToken {
			c.Tokens.Invalidate()
		}
		token, err := c.Tokens.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("obtaining token: %w", err)
		}
		req, err := c.newRequest(ctx, method, path, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return c.httpClient().Do(req)
	}

	resp, err := attempt(false)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		resp, err = attempt(true)
		if err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	return resp.Header, decodeJSONResponse(resp, out)
}

func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decodeJSONResponse(resp, out)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	url := strings.TrimRight(c.GatewayURL, "/") + path
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func decodeJSONResponse(resp *http.Response, out any) error {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(raw))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}
