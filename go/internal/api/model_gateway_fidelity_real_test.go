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

	// The model is a PARAMETER and not a closure constant because this test now exercises two
	// models on one deployment: the text one for tools and structured output, and a vision one for
	// the image. The gateway routes on the candidate, so the same server serves both.
	decideRealAs := func(t *testing.T, routeModel string, rendered map[string]any) (int, map[string]any) {
		t.Helper()
		rendered["model"] = routeModel
		return postDecide(t, srv, decideRequest{
			Candidates:      []decideCandidate{{Provider: prometheusinference.Name, Model: routeModel, Priority: 0}},
			RenderedContext: rendered,
		})
	}
	decideReal := func(t *testing.T, rendered map[string]any) (int, map[string]any) {
		t.Helper()
		return decideRealAs(t, model, rendered)
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
		// THE DEFECT THIS RECORDS, measured rather than argued.
		//
		// The old mapping did `content, _ := m["content"].(string)` — an assertion that fails SILENTLY
		// on an array — so a vision call reached the platform as a message with empty content. The
		// platform answered 200 with content "" (measured). A caller asking about an image got a
		// successful, empty answer; nothing anywhere said the image had been dropped.
		//
		// A 64x64 PNG, GREEN top half and RED bottom half, 133 bytes.
		//
		// It replaced a 1x1 TRANSPARENT pixel, and that swap is what made the first subtest below
		// possible. With a transparent pixel the only available assertion is the refusal, because not
		// even a real vision model can name a colour that is not there — so the success branch could
		// only ever have asserted "it answered something", which a model hallucinating about an image
		// it never received also satisfies. Two specific colours is the cheapest assertion that
		// DISCRIMINATES, and naming them is within reach of the smallest VLM, which is all this test
		// needs: it measures transport, not model quality.
		const greenOverRedPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAIAAAAlC+aJAAAATElEQVR42u3PMQ0AAAgDsPk3DRp2EprUQJPJbQICAgICAgICAgICAgICAgICAgICAgIC7wgICAgICAgICAgICAgICAgICAgICAgI1BaxZPDi2L1X8gAAAABJRU5ErkJggg=="

		imageMessage := func() []any {
			return []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "Name the two colours in this image, in English, " +
					"one word each."},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": greenOverRedPNG}},
			}}}
		}

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

		t.Run("a vision model SEES the image and names both of its colours", func(t *testing.T) {
			// THE ASSERTION VERITIUM ASKED FOR, and for a day it could not be made: both models this
			// client was granted were modality=text, so the only available evidence was the
			// platform's refusal. Prometheus granted three vision models on 2026-10-08
			// (qwen3-vl-8b, fara-7b, qwen3vl-30b-a3b) and the assertion is now the answer itself.
			//
			// Measured the same day, which is why qwen3-vl-8b is the default: 2.3s and "green \n red",
			// against 5.8s for qwen3vl-30b-a3b and a fara-7b that names both colours and then emits a
			// stray <tool_call> block — it is a computer-use agent model and wants to act, not answer.
			//
			// Both colours and not one: a model guessing from the prompt alone could land on either,
			// and landing on both is what the image decides.
			visionModel := realPrometheusVisionModel()
			status, body := decideRealAs(t, visionModel, map[string]any{
				"messages":   imageMessage(),
				"max_tokens": 400,
			})
			if status != http.StatusOK {
				t.Fatalf("the vision model %q refused a data-URI image: status=%d body=%v",
					visionModel, status, body)
			}
			content, _ := firstMessage(t, body)["content"].(string)
			lowered := strings.ToLower(content)
			missing := []string{}
			for _, colour := range []string{"green", "red"} {
				if !strings.Contains(lowered, colour) {
					missing = append(missing, colour)
				}
			}
			if len(missing) > 0 {
				t.Fatalf("%q answered %q without naming %v — the image was dropped and the model is "+
					"guessing, or it arrived degraded", visionModel, content, missing)
			}
			t.Logf("%s named both colours: %q", visionModel, content)
		})

		t.Run("a text model refuses the same image BY MODALITY", func(t *testing.T) {
			// The negative control, and it is permanent rather than a stand-in for the subtest above.
			//
			// It proves arrival WITHOUT depending on any model's answer: the platform can only name
			// the modality having received an image, and a request with the image dropped is not
			// refused — it succeeds with content "" (measured both ways on 2026-10-08). So this
			// subtest fails if the call quietly succeeds, which is exactly the old behaviour.
			status, body := decideReal(t, map[string]any{
				"messages":   imageMessage(),
				"max_tokens": 64,
			})
			if status == http.StatusOK {
				content, _ := firstMessage(t, body)["content"].(string)
				t.Fatalf("the text model %q accepted an image and answered %q — either it is no longer "+
					"modality=text, or the image never left this process", model, content)
			}
			errMsg, _ := body["error"].(string)
			lowered := strings.ToLower(errMsg)
			if !strings.Contains(lowered, "image") && !strings.Contains(lowered, "modality") {
				t.Fatalf("status=%d error=%q — expected a refusal naming the image or the modality; "+
					"anything else means we cannot tell whether the image arrived", status, errMsg)
			}
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
	client, model := realPrometheusClientOrSkip(t)

	gw := modelgateway.New()
	gw.RegisterProvider(prometheusinference.Name, &prometheusinference.Adapter{Client: client, Model: model})
	mux := http.NewServeMux()
	(&ModelGatewayHandlers{Gateway: gw, Pricing: finops.NewPricingTable(nil)}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux))
	t.Cleanup(srv.Close)
	return srv
}

// realPrometheusClientOrSkip builds a client against the real deployment, or skips.
//
// Shared by both of VRT-AEON-003's real-platform tests (A-2's fidelity and A-3's idempotency) so
// there is one place that decides what "the real platform is available" means. The guard names only
// the three variables the client actually needs — the older OBS-006 test still guards on
// PROMETHEUS_AUTH_URL, which the client no longer HAS (one address, not two: see Client.GatewayURL),
// so that test skips over a variable nothing reads.
func realPrometheusClientOrSkip(t *testing.T) (*prometheusinference.Client, string) {
	t.Helper()
	gatewayURL := os.Getenv("PROMETHEUS_GATEWAY_URL")
	clientID := os.Getenv("PROMETHEUS_CLIENT_ID")
	clientSecret := os.Getenv("PROMETHEUS_CLIENT_SECRET")
	if gatewayURL == "" || clientID == "" || clientSecret == "" {
		// The wording starts with the exact phrase scripts/check_skips.py tolerates, and that is not a
		// coincidence to be cleaned up later: that file's own comment records having carried three
		// variants of this message and missed the fourth. One condition, one wording.
		t.Skip("Prometheus credentials not set — set PROMETHEUS_GATEWAY_URL, PROMETHEUS_CLIENT_ID and " +
			"PROMETHEUS_CLIENT_SECRET (they live in .env, which is gitignored) and run `make test-vrt-aeon-003`")
	}
	model := realPrometheusModel()
	scope := os.Getenv("PROMETHEUS_SCOPE")
	if scope == "" {
		// Same derivation MDL-015 made the gateway do from the bundle: one model:<id> per candidate.
		// A hand-written scope naming one model while routing picks per request is what made every
		// other candidate answer 403 — and this test now routes to TWO models, so both are named.
		scope = "inference:read model:" + model + " model:" + realPrometheusVisionModel()
	}
	client := &prometheusinference.Client{
		GatewayURL:   gatewayURL,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scope:        scope,
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, model
}

// realPrometheusModel is configurable because which models a client is authorized for is a property
// of the deployment, not of this test — same reasoning as MDL-015's DEFAULT_MODEL.
func realPrometheusModel() string {
	if m := os.Getenv("PROMETHEUS_DEFAULT_MODEL"); m != "" {
		return m
	}
	return "gpt-oss-20b-mxfp4"
}

// realPrometheusVisionModel is the model the image subtest routes to. Same reasoning as above: which
// models a client is granted is a property of the deployment.
//
// The default is qwen3-vl-8b because it was the fastest and cleanest of the three vision models
// granted on 2026-10-08, measured: 2.3s naming both colours, against 5.8s for qwen3vl-30b-a3b, and a
// fara-7b that answers correctly and then emits a stray <tool_call> block.
//
// A client cannot enumerate what the deployment hosts, but it CAN read back what it was granted: a
// token request that omits `scope` comes back with the full granted scope in the response's `scope`
// field. That is how these three were found, and it is worth knowing — GET /v1/models only returns
// the models the REQUESTED scope named, so it cannot discover a grant you have not guessed.
func realPrometheusVisionModel() string {
	if m := os.Getenv("PROMETHEUS_VISION_MODEL"); m != "" {
		return m
	}
	return "qwen3-vl-8b"
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

// TestReasoningOnlyAssistantTurnDoesNotBreakTheRun is VRT-SYN-004's acceptance test, against the
// real prometheus deployment.
//
// Veritium found it against a deployment and Synaptum traced it to both sides: a reasoning model
// answers with reasoning and nothing else, the caller keeps that turn in its history, and the next
// turn's request carries an assistant message with no `content` and no `tool_calls`. Measured here
// on 2026-10-10 before the fix: `400 Assistant message must contain either 'content' or
// 'tool_calls'!`, surfaced by the gateway as "all candidates failed" — unactionable, and the run is
// lost. It is intermittent, and likelier the longer the run.
//
// AGAINST REAL INFERENCE AND NOT A DOUBLE, for the same reason as the fidelity test above: a double
// asserts the body we meant to send. The whole question here is which bodies the platform accepts,
// and every branch of unsendable.go's rule is a measurement of that. The unit test in
// internal/modelgateway pins the boundary in CI; this one is why the boundary is where it is.
func TestReasoningOnlyAssistantTurnDoesNotBreakTheRun(t *testing.T) {
	srv := newRealPrometheusGatewayServer(t)
	model := realPrometheusModel()

	ask := func(t *testing.T, messages []any) (int, map[string]any) {
		t.Helper()
		return postDecide(t, srv, decideRequest{
			Candidates:      []decideCandidate{{Provider: prometheusinference.Name, Model: model, Priority: 0}},
			RenderedContext: map[string]any{"messages": messages, "max_tokens": 64, "model": model},
		})
	}

	t.Run("a history holding a reasoning-only assistant turn is answered, not refused", func(t *testing.T) {
		status, body := ask(t, []any{
			map[string]any{"role": "user", "content": "Di 'hola' y nada mas."},
			map[string]any{"role": "assistant", "reasoning_content": "El usuario pide un saludo breve."},
			map[string]any{"role": "user", "content": "Ahora di 'adios'."},
		})
		if status != http.StatusOK {
			t.Fatalf("status = %d, body = %v — this is the 400 VRT-SYN-004 reported, reaching us as a dead run", status, body)
		}
		// REPORTED, not silently repaired. The call succeeding is half the requirement; the other
		// half is that the caller can tell its request was narrowed, because a gateway that quietly
		// drops messages is what VRT-AEON-003 was raised about.
		if got := body["unsendable_turns_dropped"]; got != float64(1) {
			t.Errorf("unsendable_turns_dropped = %v (%T), want 1: the drop must be reported in the body", got, got)
		}
	})

	t.Run("an ordinary history is untouched and reports nothing", func(t *testing.T) {
		status, body := ask(t, []any{
			map[string]any{"role": "user", "content": "Di 'hola' y nada mas."},
			map[string]any{"role": "assistant", "content": "hola"},
			map[string]any{"role": "user", "content": "Ahora di 'adios'."},
		})
		if status != http.StatusOK {
			t.Fatalf("status = %d, body = %v", status, body)
		}
		// ABSENT and not 0: absence is what tells a caller its request went out as it built it.
		if _, present := body["unsendable_turns_dropped"]; present {
			t.Errorf("unsendable_turns_dropped is present on an untouched request: %v", body["unsendable_turns_dropped"])
		}
	})

	// THE BOUNDARY, against the real platform: an explicit empty string is sendable and must survive.
	// Collapsing it with nil would turn a working request into a dropped turn, and the only way to
	// know which is which is to ask the platform.
	t.Run("an assistant turn whose content is an explicit empty string is kept", func(t *testing.T) {
		status, body := ask(t, []any{
			map[string]any{"role": "user", "content": "Di 'hola' y nada mas."},
			map[string]any{"role": "assistant", "content": ""},
			map[string]any{"role": "user", "content": "Ahora di 'adios'."},
		})
		if status != http.StatusOK {
			t.Fatalf("status = %d, body = %v", status, body)
		}
		if _, present := body["unsendable_turns_dropped"]; present {
			t.Errorf("a turn with content \"\" was dropped: %v", body["unsendable_turns_dropped"])
		}
	})
}
