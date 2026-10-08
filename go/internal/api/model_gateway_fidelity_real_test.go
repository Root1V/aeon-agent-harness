package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/finops"
	"github.com/aeon-ai/aeon/go/internal/modelgateway"
	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
)

// VRT-AEON-003 A-2's acceptance test: modelgw -> REAL Prometheus, for the request fields the adapter
// used to drop.
//
// WHY THIS CANNOT BE A DOUBLE, which is the condition we attached when we accepted the request: a
// double asserts the shape of the request we send and nothing about fidelity, and fidelity is the
// entire subject. The old mapping produced requests the platform ACCEPTED — it answered 200 — with
// the tools gone, the schema gone and the image gone. Any test that stops at "we sent what we meant
// to send" passes against both versions of this code.
//
// Measured on 2026-10-08 against the real deployment, which is where each assertion below comes from:
//
//	tools + tool_choice     -> finish_reason "tool_calls", arguments {"city":"Quito"}
//	the second leg          -> the model answers FROM the tool result ("cloudy", "17 °C")
//	response_format         -> content is "{\"city\":\"Quito\"}", valid against the schema
//	content as parts        -> answered; previously collapsed to "" by a failed type assertion
//	image_url (data URI)    -> refused: "does not support image input (modality='text')"
//
// It spends real money (fractions of a cent) on real inference, so it is excluded from CI for the
// same reason as MDL-015's: see `make test-vrt-aeon-003`.
func TestAdapterFidelityAgainstRealPrometheus(t *testing.T) {
	srv := newRealPrometheusGatewayServer(t)
	model := realPrometheusModel()

	decideReal := func(t *testing.T, rendered map[string]any) (int, map[string]any) {
		t.Helper()
		rendered["model"] = model
		return postDecide(t, srv, decideRequest{
			Candidates:      []decideCandidate{{Provider: "prometheus_inference", Model: model, Priority: 0}},
			RenderedContext: rendered,
		})
	}

	weatherTool := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "get_weather",
			"description": "Get the current weather for a city",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			},
		},
	}}

	t.Run("a tool call round-trips: the model asks, the tool answers, the model uses the answer", func(t *testing.T) {
		// LEG 1 — tools and tool_choice have to reach the platform, and the calls it returns have to
		// reach our caller. Neither half existed: chatRequestFrom copied four fields and
		// chatCompletionResponse modelled no tool_calls, so a model that answered with a call
		// produced an empty ChatResult. An agent loop reading that sees a turn with nothing said and
		// nothing to do, which is a plausible end of conversation rather than an error.
		status, body := decideReal(t, map[string]any{
			"messages":    []any{map[string]any{"role": "user", "content": "What is the weather in Quito?"}},
			"tools":       weatherTool,
			"tool_choice": "required",
			"max_tokens":  200,
		})
		if status != http.StatusOK {
			t.Fatalf("leg 1: status=%d body=%v", status, body)
		}
		message := firstMessage(t, body)

		if got := message["finish_reason"]; got != nil && got != "tool_calls" {
			t.Logf("finish_reason = %v (the platform decides this; the calls below are what matter)", got)
		}
		rawCalls, _ := message["tool_calls"].([]any)
		if len(rawCalls) == 0 {
			t.Fatal("no tool_calls in the normalized response — with tool_choice=required the platform " +
				"returned a call and we dropped it on the way out")
		}
		call, _ := rawCalls[0].(map[string]any)
		fn, _ := call["function"].(map[string]any)
		if fn["name"] != "get_weather" {
			t.Fatalf("tool call names %v, want get_weather — the tool definition did not reach the model", fn["name"])
		}
		// Arguments are a STRING on this surface (OpenAI's shape, for INT-002 consumers) and a decoded
		// map on the Go surface. Here we assert the model filled them in: the schema travelled too.
		rawArgs, _ := fn["arguments"].(string)
		var args map[string]any
		if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
			t.Fatalf("arguments %q are not JSON: %v", rawArgs, err)
		}
		if city, _ := args["city"].(string); !strings.EqualFold(city, "Quito") {
			t.Fatalf("arguments = %v, want city=Quito — the parameter schema did not reach the model", args)
		}
		callID, _ := call["id"].(string)
		if callID == "" {
			t.Fatal("the tool call has no id, so no tool result can ever be paired with it")
		}

		// LEG 2 — the half that makes leg 1 worth anything. Replaying the assistant turn requires
		// Message.ToolCalls on INPUT, and the result requires role=tool with tool_call_id. Without
		// either, a conversation cannot continue past its first tool call.
		status, body = decideReal(t, map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": "What is the weather in Quito?"},
				map[string]any{"role": "assistant", "content": nil, "tool_calls": rawCalls},
				map[string]any{"role": "tool", "tool_call_id": callID, "content": `{"temp_c": 17, "sky": "cloudy"}`},
			},
			"tools":      weatherTool,
			"max_tokens": 200,
		})
		if status != http.StatusOK {
			t.Fatalf("leg 2: status=%d body=%v", status, body)
		}
		answer, _ := firstMessage(t, body)["content"].(string)
		// The model must have used the DATA the tool returned. Asserting on the numbers and not on
		// "it answered something" is what distinguishes a paired result from a dropped one: with the
		// tool message missing or unpaired the platform still answers, about the weather in general.
		if !strings.Contains(answer, "17") || !strings.Contains(strings.ToLower(answer), "cloud") {
			t.Fatalf("the final answer does not use the tool's result (temp_c=17, sky=cloudy): %q", answer)
		}
	})

	t.Run("a json_schema response_format reaches the platform and constrains the answer", func(t *testing.T) {
		// response_format is the field named in chatRequestFrom's own comment as the one Synaptum lost
		// an afternoon to, and it was still being dropped when that comment was written.
		status, body := decideReal(t, map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "Give me the capital of Ecuador."}},
			"response_format": map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   "capital",
					"strict": true,
					"schema": map[string]any{
						"type":                 "object",
						"properties":           map[string]any{"city": map[string]any{"type": "string"}},
						"required":             []any{"city"},
						"additionalProperties": false,
					},
				},
			},
			"max_tokens": 200,
		})
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%v", status, body)
		}
		content, _ := firstMessage(t, body)["content"].(string)
		var parsed map[string]any
		if err := json.Unmarshal([]byte(content), &parsed); err != nil {
			// This is the observable difference: without response_format the model answers in prose,
			// which is exactly what a caller asking for structured output cannot use.
			t.Fatalf("the answer is not JSON (%q): %v — response_format did not reach the platform", content, err)
		}
		if _, ok := parsed["city"].(string); !ok {
			t.Fatalf("the answer %v does not satisfy the schema (requires a string `city`)", parsed)
		}
		if len(parsed) != 1 {
			t.Errorf("the answer %v carries fields the schema forbids (additionalProperties: false)", parsed)
		}
	})

	t.Run("chat_template_kwargs reaches the platform", func(t *testing.T) {
		// A valid kwarg is accepted...
		status, body := decideReal(t, map[string]any{
			"messages":             []any{map[string]any{"role": "user", "content": "Name the capital of Ecuador."}},
			"chat_template_kwargs": map[string]any{"reasoning_effort": "low"},
			"max_tokens":           400,
		})
		if status != http.StatusOK {
			t.Fatalf("a valid chat_template_kwargs was refused: status=%d body=%v", status, body)
		}

		// ...and THIS is the assertion that can actually fail, which is the only reason the subtest
		// is worth running. The first version asserted only the 200 above, and the negative control
		// exposed it at once: with the passthrough deleted the request still succeeds, so the subtest
		// passed while naming a field that never left the process. A test that cannot fail when the
		// thing it names is removed is naming something else.
		//
		// A deliberately ill-typed value makes the platform refuse and NAME THE FIELD
		// ("body.chat_template_kwargs: Input should be a valid dictionary", measured). It can only
		// name a field it received; drop the passthrough and the same call returns 200. Same shape of
		// evidence as the image below — a refusal is proof of arrival where a success is not.
		status, body = decideReal(t, map[string]any{
			"messages":             []any{map[string]any{"role": "user", "content": "Say OK."}},
			"chat_template_kwargs": "not an object, deliberately",
			"max_tokens":           64,
		})
		if status == http.StatusOK {
			t.Fatal("the platform accepted an ill-typed chat_template_kwargs, which means it never " +
				"arrived — the field is being dropped before the wire")
		}
		errMsg, _ := body["error"].(string)
		if !strings.Contains(errMsg, "chat_template_kwargs") {
			t.Fatalf("the refusal does not name chat_template_kwargs (%q), so it is not evidence that "+
				"the field arrived", errMsg)
		}
	})

	t.Run("content as parts is transported, and an image really arrives", func(t *testing.T) {
		// THE FINDING THIS SUBTEST RECORDS, measured rather than argued.
		//
		// The old mapping did `content, _ := m["content"].(string)` — an assertion that fails SILENTLY
		// on an array — so a vision call reached the platform as a message with empty content. The
		// platform answered 200 with content "" (measured). A caller asking about an image got a
		// successful, empty answer; nothing anywhere said the image had been dropped.
		//
		// The deployment we have exposes two models and BOTH are modality=text (GET /v1/models/mine,
		// measured the same day), so there is no vision-capable model here to answer a question about
		// an image. That is a property of the deployment, not of this mapping, and we do not get to
		// assert past it: a double would be the only way to "pass" a vision assertion, and a double
		// proves nothing about fidelity.
		//
		// What CAN be verified against the real platform is strictly stronger than a happy answer: the
		// platform's refusal names the modality, which it can only do because it RECEIVED an image. A
		// request with the image dropped does not get refused — it succeeds. So the refusal is the
		// evidence, and the subtest below fails if the call quietly succeeds.
		const onePixelPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

		t.Run("a text-only parts array is read, not just accepted", func(t *testing.T) {
			// The prompt asks for a WORD THAT ONLY APPEARS IN THE PART, so the assertion separates
			// "the text arrived" from "the platform accepted the request". An empty user message also
			// produces a 200 — that is the whole defect — and a model answering anything at all to
			// nothing would pass a check for non-empty content.
			status, body := decideReal(t, map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": "Reply with exactly the word PICHINCHA and nothing else."},
				}}},
				// 400 and not 32. The first version of this subtest used 32 and FAILED against the
				// real platform with empty content: this is a reasoning model, it spent the whole
				// allowance on its chain of thought, and nothing was left for the answer. MDL-015
				// documented that hazard and this test walked into it anyway. The failure message
				// below now tells the two apart instead of blaming the mapping for a budget.
				"max_tokens": 400,
			})
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%v", status, body)
			}
			message := firstMessage(t, body)
			content, _ := message["content"].(string)
			if !strings.Contains(strings.ToUpper(content), "PICHINCHA") {
				reasoning, _ := message["reasoning_content"].(string)
				if strings.TrimSpace(content) == "" && strings.TrimSpace(reasoning) != "" {
					t.Fatalf("empty answer with a non-empty chain of thought (%d chars) and "+
						"finish_reason=%v: the model ran out of token budget, which is this test's "+
						"construction and not the mapping", len(reasoning), message["finish_reason"])
				}
				t.Fatalf("the answer %q does not contain the word that only the text part carried — "+
					"the parts array was dropped (content, _ := m[\"content\"].(string) fails silently "+
					"on an array, and the platform answers 200 to the empty message that leaves)", content)
			}
		})

		t.Run("an image part arrives at the platform", func(t *testing.T) {
			status, body := decideReal(t, map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": "What colour is this image?"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": onePixelPNG}},
				}}},
				"max_tokens": 64,
			})
			if status == http.StatusOK {
				content, _ := firstMessage(t, body)["content"].(string)
				// Two ways to get here. Either the deployment gained a vision-capable model, in which
				// case this is a pass and the skip below should be revisited — or the image was
				// dropped again and a text model answered about nothing, which is the defect.
				if strings.TrimSpace(content) == "" {
					t.Fatalf("the platform answered a vision request with empty content — the image " +
						"was dropped on the way out")
				}
				t.Logf("a vision-capable model answered: %q — the deployment is no longer text-only, "+
					"and this subtest can assert the answer rather than the refusal", content)
				return
			}
			// The expected outcome on a text-only deployment, and the one that proves transport.
			errMsg, _ := body["error"].(string)
			lowered := strings.ToLower(errMsg)
			if !strings.Contains(lowered, "image") && !strings.Contains(lowered, "modality") {
				t.Fatalf("status=%d error=%q — expected either an answer or a refusal naming the image; "+
					"anything else means we cannot tell whether the image arrived", status, errMsg)
			}
			t.Logf("the platform refused BY MODALITY (%q), which it can only do having received the "+
				"image: a request with the image dropped succeeds instead", errMsg)
		})
	})
}

// newRealPrometheusGatewayServer mounts the REAL ModelGatewayHandlers over the REAL gateway with the
// REAL prometheus_inference adapter, and skips when the deployment's credentials are absent.
//
// No ledger: this test is about request/response fidelity, and leaving Ledger nil means no Postgres
// is needed to run it. recordCost already tolerates that (it returns before writing), which is the
// behaviour a gateway deployed without a ledger has.
func newRealPrometheusGatewayServer(t *testing.T) *httptest.Server {
	t.Helper()
	gatewayURL := os.Getenv("PROMETHEUS_GATEWAY_URL")
	clientID := os.Getenv("PROMETHEUS_CLIENT_ID")
	clientSecret := os.Getenv("PROMETHEUS_CLIENT_SECRET")
	if gatewayURL == "" || clientID == "" || clientSecret == "" {
		t.Skip("real-platform test: set PROMETHEUS_GATEWAY_URL, PROMETHEUS_CLIENT_ID and " +
			"PROMETHEUS_CLIENT_SECRET (they live in .env, which is gitignored) — see `make test-vrt-aeon-003`")
	}
	model := realPrometheusModel()
	scope := os.Getenv("PROMETHEUS_SCOPE")
	if scope == "" {
		// Same derivation MDL-015 made the gateway do from the bundle: one model:<id> per candidate.
		// A hand-written scope naming one model while routing picks per request is what made every
		// other candidate answer 403.
		scope = "inference:read model:" + model
	}
	client := &prometheusinference.Client{
		GatewayURL:   gatewayURL,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scope:        scope,
	}
	t.Cleanup(func() { _ = client.Close() })

	gw := modelgateway.New()
	gw.RegisterProvider("prometheus_inference", &prometheusinference.Adapter{Client: client, Model: model})
	mux := http.NewServeMux()
	(&ModelGatewayHandlers{Gateway: gw, Pricing: finops.NewPricingTable(nil)}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux))
	t.Cleanup(srv.Close)
	return srv
}

// realPrometheusModel is configurable because which models a client is authorized for is a property
// of the deployment, not of this test — same reasoning as MDL-015's DEFAULT_MODEL.
func realPrometheusModel() string {
	if m := os.Getenv("PROMETHEUS_DEFAULT_MODEL"); m != "" {
		return m
	}
	return "gpt-oss-20b-mxfp4"
}

// firstMessage digs the assistant message out of a /decide response, failing on any shape that is
// not the normalized one every adapter returns.
func firstMessage(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	output, ok := body["output"].(map[string]any)
	if !ok {
		t.Fatalf("response carries no `output` object: %v", body)
	}
	choices, _ := output["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in %v", output)
	}
	choice, _ := choices[0].(map[string]any)
	message, ok := choice["message"].(map[string]any)
	if !ok {
		t.Fatalf("choice carries no `message` object: %v", choice)
	}
	// finish_reason lives on the choice, but every assertion that wants it wants it next to the
	// message, so it is copied in rather than returned separately.
	if fr, present := choice["finish_reason"]; present {
		message["finish_reason"] = fr
	}
	return message
}
