package prometheusinference

import (
	"context"
	"fmt"

	"github.com/Root1V/axonium-sdk/go/axonium"
)

// Client is Aeon's seam onto the Prometheus platform. Since MDL-009 the transport underneath it is
// Axonium's Go SDK instead of ~400 lines of our own (client.go + auth.go, both deleted).
//
// What went away: token minting and refresh, the 401-then-retry-with-a-fresh-token path, header
// assembly, idempotency-key plumbing, and response-header parsing for request ids and replay markers.
// All of that is contract-shaped, so it belongs to whoever owns the contract; a second Go
// implementation of it only created a second place for it to drift.
//
// What deliberately stayed OURS: normalization. The SDK's own doc draws the line in the same place —
// "the SDK is pure transport […] normalization belongs above it, in whatever framework is consuming
// the models, so that there is one implementation of that vocabulary rather than one per language
// SDK." This file therefore maps SDK types onto Aeon's vocabulary and does nothing else.
//
// The acceptance condition we set before agreeing to depend on them was met and MEASURED rather than
// asserted: cancellation really stops the upstream ("upstream stopped after 3 chunks; the client read
// 3"), through both doors, and 24/24 cases of the shared corpus reproduce byte-identical output.
type Client struct {
	// GatewayURL is the platform address — ONE address, not two.
	//
	// The old AuthURL field is gone rather than kept as an option. Verified on this deployment: both
	// :8020 and :9000 answer /oauth2/token, so the gateway really does serve tokens too. Axonium
	// removed the same field from their SDK for the failure it caused — pointing a client at your own
	// deployment meant changing TWO addresses, and forgetting the second left it asking the official
	// platform for a token and using it elsewhere. Nothing failed; the token was issued by the wrong
	// party.
	GatewayURL string
	ClientID   string
	// ClientSecret is never logged and never returned.
	ClientSecret string
	// Scope is the space-separated scope request. Requesting a scope the account lacks is an error
	// rather than a silent downgrade, so read back what was granted.
	Scope string

	sdk *axonium.Client
}

// client lazily builds the SDK client. Lazy on purpose: constructing it validates configuration, and a
// process that only ever routes to another provider must not fail at startup over credentials it will
// never use.
func (c *Client) client() (*axonium.Client, error) {
	if c.sdk != nil {
		return c.sdk, nil
	}
	sdk, err := axonium.New(axonium.Config{
		GatewayBaseURL: c.GatewayURL,
		ClientID:       c.ClientID,
		ClientSecret:   c.ClientSecret,
		Scope:          c.Scope,
	})
	if err != nil {
		return nil, fmt.Errorf("prometheus_inference: building the Axonium client: %w", err)
	}
	c.sdk = sdk
	return sdk, nil
}

// Close releases the underlying SDK client.
func (c *Client) Close() error {
	if c.sdk == nil {
		return nil
	}
	return c.sdk.Close()
}

// IdempotencyKeyField is the request-map key a caller uses to ask for an idempotent call (OBS-006).
// It is lifted out of the body and handed to the SDK, which sends it as a header — left in the body
// the gateway would discard it in silence, and a caller would believe it had asked for idempotency
// while getting a fresh generation every time.
const IdempotencyKeyField = "idempotency_key"

// CallMeta is what Aeon needs about a response beyond its content (OBS-005/006/007).
type CallMeta struct {
	// RequestID is the platform's id for THIS call, and the join key between our cost ledger and
	// theirs (OBS-007).
	RequestID string
	// IdempotentReplayOf names the generation that was actually billed, when this response was
	// replayed rather than generated (OBS-006). Empty on a real generation. The SDK's doc states
	// independently what we measured: a replay's Usage describes the ORIGINAL generation, and the
	// replay's own request id has no usage row (404) because replaying reaches no model.
	IdempotentReplayOf string
	// InstanceID is the deployment that actually served this call (OBS-005). The ledger imputes cost
	// by the bundle's model, which is right — you pay for the profile, not the deployment — but which
	// deployment answered is where an incident starts, and it was recorded nowhere.
	InstanceID string
	// WaitedForSeconds is how long the SDK slept honouring a Retry-After. Surfaced because a wait that
	// exists only as a log line is invisible: three teams reported a 36-second call as a hang. It is
	// NOT service time — subtract it before attributing latency to the platform.
	WaitedForSeconds float64
}

// ChatCompletion sends a chat request and returns the raw decoded body.
func (c *Client) ChatCompletion(ctx context.Context, requestBody map[string]any) (map[string]any, error) {
	raw, _, err := c.ChatCompletionWithMeta(ctx, requestBody)
	return raw, err
}

// ChatCompletionWithMeta also returns the platform's own metadata for the call.
func (c *Client) ChatCompletionWithMeta(ctx context.Context, requestBody map[string]any) (map[string]any, CallMeta, error) {
	sdk, err := c.client()
	if err != nil {
		return nil, CallMeta{}, err
	}

	key, _ := requestBody[IdempotencyKeyField].(string)
	req, err := chatRequestFrom(requestBody, key)
	if err != nil {
		return nil, CallMeta{}, err
	}

	completion, err := sdk.Chat.Create(ctx, req)
	if err != nil {
		return nil, CallMeta{}, fmt.Errorf("prometheus_inference: chat completion: %w", err)
	}

	meta := CallMeta{
		RequestID:        completion.Meta.RequestID,
		InstanceID:       completion.Meta.InstanceID,
		WaitedForSeconds: completion.Meta.WaitedFor.Seconds(),
	}
	// Read the replay marker from the id, not from the boolean alone: a true with no id would say a
	// replay happened without saying of what, and the id is the only part a caller can act on.
	if completion.Meta.IdempotentReplay {
		meta.IdempotentReplayOf = completion.Meta.IdempotentReplayOf
	}

	// Raw is the decoded body as received, and normalization reads THAT rather than the SDK's typed
	// accessors. One parser for this vocabulary — ours — so a field the SDK does not model stays
	// reachable instead of silently dropped.
	if completion.Raw == nil {
		return nil, meta, fmt.Errorf("prometheus_inference: the SDK returned no raw body for request %s", meta.RequestID)
	}
	return completion.Raw, meta, nil
}

// UsageRecord is the platform's own accounting for ONE request (OBS-007), read through the SDK.
type UsageRecord struct {
	RequestID   string
	Model       string
	RequestKind string
	// PromptTokens/CompletionTokens/CachedTokens mirror the inference response field for field, and
	// CachedTokens is a SUBSET of PromptTokens — the convention agreed with both other teams, so a row
	// and the response it describes compare without arithmetic.
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	// TerminationReason stays a plain string all the way through. The platform proposed a fourth value
	// and withdrew it; an exhaustive switch would turn the next one into a failure for a caller that
	// only wanted token counts.
	TerminationReason string
	// CostUSD is nil where no price is configured, which is NOT free. Eleven rows on this deployment
	// are null, created when two modalities were added to the registry and not to the price table, and
	// deliberately left null rather than rewritten (OBS-008).
	CostUSD *float64
	// PromptPricePer1M/CompletionPricePer1M are the rates ACTUALLY APPLIED, and they are what make
	// reconciliation a comparison of two derivations instead of a copy of one number (OBS-007).
	//
	// Read out of Raw because the SDK models cost_usd and not rates. Not a workaround: Raw exists for
	// exactly this, and its doc says new columns are appended over time. Worth asking Axonium to model
	// it, by their own argument — the rates are what let a client check the charge instead of trusting
	// it.
	PromptPricePer1M     *float64
	CompletionPricePer1M *float64
	InstanceID           string
}

// Usage fetches the platform's accounting for one request.
func (c *Client) Usage(ctx context.Context, requestID string) (*UsageRecord, error) {
	if requestID == "" {
		return nil, fmt.Errorf("prometheus_inference: usage: empty request id")
	}
	sdk, err := c.client()
	if err != nil {
		return nil, err
	}
	row, err := sdk.Usage.Retrieve(ctx, requestID)
	if err != nil {
		return nil, fmt.Errorf("prometheus_inference: usage for %s: %w", requestID, err)
	}

	out := &UsageRecord{
		RequestID:         row.RequestID,
		Model:             row.Model,
		RequestKind:       row.RequestKind,
		TerminationReason: row.TerminationReason,
		CostUSD:           row.CostUSD,
		InstanceID:        row.InstanceID,
	}
	if row.Usage != nil {
		out.PromptTokens = derefInt(row.Usage.PromptTokens)
		out.CompletionTokens = derefInt(row.Usage.CompletionTokens)
		out.CachedTokens = derefInt(row.Usage.CacheReadTokens)
	}
	out.PromptPricePer1M, out.CompletionPricePer1M = ratesFromRaw(row.Raw)
	return out, nil
}

// ratesFromRaw pulls the applied rates out of the unmodelled part of the row.
func ratesFromRaw(raw map[string]any) (prompt, completion *float64) {
	rates, _ := raw["rates"].(map[string]any)
	if rates == nil {
		return nil, nil
	}
	return optionalFloat(rates["prompt_price_per_1m"]), optionalFloat(rates["completion_price_per_1m"])
}

// optionalFloat returns nil for an absent or non-numeric value, so "no rate configured" stays
// distinguishable from a rate of zero.
func optionalFloat(v any) *float64 {
	f, ok := v.(float64)
	if !ok {
		return nil
	}
	return &f
}

// derefInt reads a pointer counter as an int. The SDK types every counter as *int because nil and zero
// mean different things — an embedding response carries no CompletionTokens because there is no
// generation, which is not the same as zero completion tokens. UsageRecord flattens them for the
// reconciler, which compares token COUNTS and treats absent as zero on purpose: a row that reports no
// completion tokens for a chat call is a divergence the caller should see as 0 vs N, not as missing.
func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// EmbeddingItem is one vector with the index the API assigned it.
type EmbeddingItem struct {
	Index     int
	Embedding []float64
}

// EmbeddingBatch is an embeddings response.
type EmbeddingBatch struct {
	Data []EmbeddingItem
	Meta CallMeta
}

// Embeddings asks for one vector per input text.
func (c *Client) Embeddings(ctx context.Context, model string, texts []string) (*EmbeddingBatch, error) {
	sdk, err := c.client()
	if err != nil {
		return nil, err
	}
	list, err := sdk.Embeddings.Create(ctx, axonium.EmbeddingRequest{Model: model, Input: texts})
	if err != nil {
		return nil, err
	}
	out := &EmbeddingBatch{
		Data: make([]EmbeddingItem, 0, len(list.Data)),
		Meta: CallMeta{RequestID: list.Meta.RequestID, InstanceID: list.Meta.InstanceID},
	}
	for _, item := range list.Data {
		// The index is carried through rather than assumed from position. The API returns an explicit
		// index per object precisely because order is not promised, and a silently mis-ordered batch
		// would attach every passage's vector to its neighbour — retrieval would still work and return
		// the wrong passages with high confidence.
		out.Data = append(out.Data, EmbeddingItem{Index: item.Index, Embedding: item.Embedding})
	}
	return out, nil
}

// Model is one entry of the platform catalog.
type Model struct {
	ID       string
	Modality string
}

// ListModels returns the models THIS CLIENT'S TOKEN IS SCOPED FOR — not the platform catalogue,
// despite the endpoint's name.
//
// Measured 2026-10-08: narrowing the requested scope narrows the result (0, 1 and 2 models for
// `inference:read`, plus one and two `model:<id>` grants), and it returned the same content as
// ListMyModels in every probe. ADR-0004 documented the opposite — "public, every active model on
// the gateway, regardless of who's authorized" — and that sentence is why we read two text-only
// models as "this deployment has no vision model" instead of "none is granted to us".
//
// TO DISCOVER WHAT THIS CLIENT HOLDS, ask for a token with no `scope` parameter: the response's own
// `scope` field carries the full granted scope. Measured 2026-10-08 — it returned six models where
// this endpoint had been showing two, three of them modality=vision. A scope-filtered catalogue
// cannot reveal a grant whose model id you have not already guessed, so the token is the only
// discovery path. What the DEPLOYMENT hosts remains invisible; only its operator can say.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	sdk, err := c.client()
	if err != nil {
		return nil, err
	}
	list, err := sdk.Models.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("prometheus_inference: listing models: %w", err)
	}
	return modelsFrom(list), nil
}

// ListMyModels returns the models this client's token actually carries.
//
// Worth asking separately, and confirmed live: being AUTHORISED for a model does not put it in the
// token. A token requested without model:<id> comes back perfectly valid and this list comes back
// empty — which is the shape of the 403 that MDL-015 spent an afternoon on.
func (c *Client) ListMyModels(ctx context.Context) ([]Model, error) {
	sdk, err := c.client()
	if err != nil {
		return nil, err
	}
	list, err := sdk.Models.Mine(ctx)
	if err != nil {
		return nil, fmt.Errorf("prometheus_inference: listing my models: %w", err)
	}
	return modelsFrom(list), nil
}

func modelsFrom(list *axonium.ModelList) []Model {
	if list == nil {
		return nil
	}
	out := make([]Model, 0, len(list.Data))
	for _, m := range list.Data {
		out = append(out, Model{ID: m.ID, Modality: m.Modality})
	}
	return out
}

// chatRequestFrom maps Aeon's rendered context onto the SDK's typed request.
//
// Unknown keys are dropped rather than forwarded, and that is deliberate narrowing: the gateway's
// request schema is an allowlist that discards what it does not recognise IN SILENCE, so forwarding a
// field would let a caller believe a constraint travelled when it did not. Synaptum lost an afternoon
// to exactly that with response_format. A field that matters gets mapped explicitly or it does not go.
func chatRequestFrom(body map[string]any, idempotencyKey string) (axonium.ChatRequest, error) {
	req := axonium.ChatRequest{IdempotencyKey: idempotencyKey}

	model, _ := body["model"].(string)
	if model == "" {
		return req, fmt.Errorf("prometheus_inference: no model in the rendered context")
	}
	req.Model = model

	rawMessages, _ := body["messages"].([]any)
	for i, rm := range rawMessages {
		m, ok := rm.(map[string]any)
		if !ok {
			return req, fmt.Errorf("prometheus_inference: message %d is not an object", i)
		}
		msg, err := messageFrom(m)
		if err != nil {
			return req, fmt.Errorf("prometheus_inference: message %d: %w", i, err)
		}
		req.Messages = append(req.Messages, msg)
	}
	if len(req.Messages) == 0 {
		return req, fmt.Errorf("prometheus_inference: no messages in the rendered context")
	}

	if v, ok := asInt(body["max_tokens"]); ok {
		req.MaxTokens = &v
	}
	if t, ok := body["temperature"].(float64); ok {
		req.Temperature = &t
	}
	if t, ok := body["top_p"].(float64); ok {
		req.TopP = &t
	}
	if v, ok := asInt(body["seed"]); ok {
		req.Seed = &v
	}

	// VRT-AEON-003 A-2: the fields an agent with native tool calling, structured output or vision
	// cannot work without. The narrowing above is still an allowlist — that part was right — but the
	// list had stopped at four fields while the contract underneath carried twelve, so any agent
	// using tools, a json_schema or an image was silently served a request with the constraint
	// removed. Veritium reported it; the comment at the top of this function already recorded that
	// Synaptum had lost an afternoon to exactly this with response_format.
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		req.Tools = tools
	}
	if tc, ok := body["tool_choice"]; ok && tc != nil {
		req.ToolChoice = tc
	}
	// response_format and chat_template_kwargs travel through Extra, which the SDK merges into the
	// encoded object (ChatRequest.MarshalJSON) — they are gateway-level fields it does not model, and
	// forwarding them verbatim is right precisely because we do not interpret them.
	for _, passthrough := range []string{"response_format", "chat_template_kwargs"} {
		if v, ok := body[passthrough]; ok && v != nil {
			if req.Extra == nil {
				req.Extra = map[string]any{}
			}
			req.Extra[passthrough] = v
		}
	}
	return req, nil
}

// messageFrom maps one rendered message onto the SDK's typed message.
//
// CONTENT IS `any`, not a string, and that is the whole vision half of A-2: the OpenAI shape allows
// content to be an ARRAY OF PARTS (`{"type":"text"}` / `{"type":"image_url"}`), and the previous
// version did `content, _ := m["content"].(string)` — a type assertion that fails silently on an
// array, so a vision call arrived at the platform with its image gone and its text gone too, as a
// message with empty content. A model answering a question about an image it was never shown is the
// worst available failure: it answers.
//
// A `role=tool` message carries tool_call_id, and an assistant message can carry tool_calls. Both
// are required to continue a conversation after a tool ran: without the id the platform cannot pair
// the result with the call, and without the calls the assistant's own turn is missing from the
// history it is asked to continue.
func messageFrom(m map[string]any) (axonium.Message, error) {
	role, _ := m["role"].(string)
	if role == "" {
		return axonium.Message{}, fmt.Errorf("no role")
	}
	msg := axonium.Message{Role: role}

	switch content := m["content"].(type) {
	case nil:
		// Legal: an assistant turn that only called tools has no content. Left nil rather than set
		// to "", so "said nothing" and "said the empty string" stay distinguishable on the wire.
	case string:
		msg.Content = content
	case []any:
		msg.Content = content
	default:
		return axonium.Message{}, fmt.Errorf("content is %T, want a string or an array of parts", content)
	}

	if id, ok := m["tool_call_id"].(string); ok && id != "" {
		msg.ToolCallID = id
	}
	if reasoning, ok := m["reasoning_content"].(string); ok && reasoning != "" {
		msg.ReasoningContent = reasoning
	}
	if rawCalls, ok := m["tool_calls"].([]any); ok {
		for _, rc := range rawCalls {
			c, ok := rc.(map[string]any)
			if !ok {
				return axonium.Message{}, fmt.Errorf("tool_calls holds a %T, want objects", rc)
			}
			call := axonium.ToolCall{}
			call.ID, _ = c["id"].(string)
			call.Type, _ = c["type"].(string)
			if call.Type == "" {
				// The only value the gateway forwards today, and the field exists so it can grow —
				// defaulted rather than demanded, because a caller replaying an assistant turn it
				// received from us should not have to know this.
				call.Type = "function"
			}
			if fn, ok := c["function"].(map[string]any); ok {
				call.Function.Name, _ = fn["name"].(string)
				call.Function.Arguments, _ = fn["arguments"].(string)
			}
			msg.ToolCalls = append(msg.ToolCalls, call)
		}
	}
	return msg, nil
}

// asInt accepts a real int (in-process callers) or a float64 (anything that went through JSON).
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}
