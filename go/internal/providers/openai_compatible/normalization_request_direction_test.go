package openaicompatible

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/modelgateway"
)

// The request direction of the shared normalization corpus (INT-012b), declared by Synaptum in the
// channel on 2026-10-14.
//
// WHY IT EXISTS AT ALL, in their words and ours: the two directions are not independent. The response
// direction PRODUCES the part (a reasoning-only assistant turn) that the request direction does not
// know how to send, so the golden corpus both sides run could not see the defect in either language —
// and it surfaced against a live deployment instead (VRT-SYN-004, and our MDL-022/DR-006).
//
// AND WHY THE DECLARATION CAME THROUGH THE CHANNEL. They wrote it in the shared `spec.md` on the 14th
// and we could not see it, because we VENDOR that spec inside this repository: two copies, and our
// copy is what this runner reads. Their sentence is the one to keep: "una declaración que el otro no
// puede ver no está declarada". Both teams had written that warning twice without acting on it.
type wireExpect struct {
	// WireRoles are the `role` values of `messages`, in order. It is what gives away an envelope that
	// is no longer there.
	WireRoles *[]string `json:"wire_roles"`
	// WireAssistantContents is each assistant message's `content`, in order. POINTERS, because `""`
	// and `null` must compare DIFFERENTLY — that distinction is half the rule, and measured against
	// the real platform: an explicit empty string is accepted, an absent one comes back 400.
	WireAssistantContents *[]*string `json:"wire_assistant_contents"`
	// WireAssistantReasoning is each assistant message's `reasoning_content`, with null where the key
	// is absent.
	WireAssistantReasoning *[]*string `json:"wire_assistant_reasoning"`
	// AssistantMessagesSendable asserts, for EVERY assistant message in the body, that `content` is a
	// string (empty counts) or there are `tool_calls`.
	//
	// OVER THE WHOLE BODY AND NOT OVER THE CASE'S MESSAGE, which is Synaptum's operational point: the
	// 400 is provoked by any assistant message that goes out with neither key, and by the time it
	// arrives the guilty turn has already passed.
	AssistantMessagesSendable *bool `json:"assistant_messages_sendable"`
}

// projectRequest returns the body our path actually puts on the wire for a shared-vocabulary request.
//
// CAPTURED FROM A RECORDING UPSTREAM, not computed by calling the transformation. What the contract
// asks about is the bytes that leave this process, and the two are not the same claim: the gateway
// applies MDL-022's rule before the adapter serializes, so a projection that called either one alone
// would describe half the path. This drives the real Gateway into the real adapter and reads what
// arrived.
func projectRequest(t *testing.T, sharedRequest []byte) map[string]any {
	t.Helper()

	var rendered map[string]any
	if err := json.Unmarshal(sharedRequest, &rendered); err != nil {
		t.Fatalf("the body_file is not a JSON object: %v", err)
	}
	if _, ok := rendered["messages"]; !ok {
		// Named rather than assumed: `spec.md` says the request direction is NOT normalized — one
		// schema, the shared vocabulary, which on our side is the rendered context. If a published
		// case turns out to carry a different shape, this is the message that should say so instead
		// of a silent empty projection.
		t.Fatalf("the body_file has no `messages`: this runner reads a body_file as the shared-vocabulary " +
			"request (the rendered context), which is what spec.md's \"la dirección request no se normaliza\" " +
			"means on our side. If the published case uses another shape, that assumption is what has to change")
	}

	captured := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"wire-probe","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)

	gw := modelgateway.New()
	gw.RegisterProvider(Name, &Adapter{BaseURL: upstream.URL})
	if _, err := gw.Decide(context.Background(),
		[]modelgateway.Candidate{{Provider: Name, Model: "wire-probe", Priority: 0}},
		rendered, ""); err != nil {
		t.Fatalf("driving the real path: %v", err)
	}

	var onTheWire map[string]any
	if err := json.Unmarshal(<-captured, &onTheWire); err != nil {
		t.Fatalf("the body that left the process is not JSON: %v", err)
	}
	return onTheWire
}

// assertWireExpectation compares the four keys of the request direction.
func assertWireExpectation(t *testing.T, caseName string, raw json.RawMessage, onTheWire map[string]any) {
	t.Helper()
	var want wireExpect
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("case %q: parsing expectation: %v", caseName, err)
	}

	messages, _ := onTheWire["messages"].([]any)
	var roles []string
	var contents, reasoning []*string
	unsendable := 0
	for _, rm := range messages {
		m, _ := rm.(map[string]any)
		role, _ := m["role"].(string)
		roles = append(roles, role)
		if role != "assistant" {
			continue
		}
		contents = append(contents, stringOrNil(m["content"]))
		reasoning = append(reasoning, stringOrNil(m["reasoning_content"]))
		calls, _ := m["tool_calls"].([]any)
		if stringOrNil(m["content"]) == nil && len(calls) == 0 {
			unsendable++
		}
	}

	if want.WireRoles != nil && !equalStrings(roles, *want.WireRoles) {
		t.Errorf("wire_roles = %v, want %v", roles, *want.WireRoles)
	}
	if want.WireAssistantContents != nil && !equalStringPtrs(contents, *want.WireAssistantContents) {
		t.Errorf("wire_assistant_contents = %v, want %v — remember \"\" and null are different here",
			showPtrs(contents), showPtrs(*want.WireAssistantContents))
	}
	if want.WireAssistantReasoning != nil && !equalStringPtrs(reasoning, *want.WireAssistantReasoning) {
		t.Errorf("wire_assistant_reasoning = %v, want %v", showPtrs(reasoning), showPtrs(*want.WireAssistantReasoning))
	}
	if want.AssistantMessagesSendable != nil {
		sendable := unsendable == 0
		if sendable != *want.AssistantMessagesSendable {
			t.Errorf("assistant_messages_sendable = %t, want %t: %d assistant message(s) went out with "+
				"neither content nor tool_calls, and any one of them provokes the 400",
				sendable, *want.AssistantMessagesSendable, unsendable)
		}
	}
}

// stringOrNil keeps the distinction the contract depends on: a key that is absent or null is nil, and
// an empty string is a pointer to "".
func stringOrNil(v any) *string {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	return &s
}

func equalStringPtrs(got, want []*string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		switch {
		case got[i] == nil && want[i] == nil:
		case got[i] == nil || want[i] == nil:
			return false
		case *got[i] != *want[i]:
			return false
		}
	}
	return true
}

func showPtrs(ps []*string) string {
	out := "["
	for i, p := range ps {
		if i > 0 {
			out += " "
		}
		if p == nil {
			out += "null"
		} else {
			out += fmt.Sprintf("%q", *p)
		}
	}
	return out + "]"
}

// TestTheRequestDirectionOnTheShapeThisLayerRECEIVES is INT-012c's half of the request direction.
//
// WHY THIS TEST EXISTS AT ALL, and it is the finding rather than the test: the three published cases
// of the request direction are written in the SHARED VOCABULARY — `content` as a list of typed parts
// — and nothing in our stack converts that to a wire body. Our gateway is an OpenAI-compatible façade:
// its callers are frameworks that build the wire body themselves, and we forward it. So the corpus
// reports those three as gaps, with their reason, and they stay gaps until someone decides our
// gateway should also accept the vocabulary.
//
// What our layer CAN be held to is what it does to a body already in the dialect, and that is three
// properties, not zero:
//
//   - it DROPS an assistant turn with neither content nor tool_calls (MDL-022's rule, which is the
//     spec's first request rule applied one layer out);
//   - it PRESERVES `content: ""`, which is the same rule's other half — the string is sendable and
//     says the assistant said nothing, which is a different claim from having nothing to send;
//   - it PRESERVES `reasoning_content`, which is the spec's third rule; we never construct it, and a
//     transport that dropped it would reproduce PRM-VRT-003's defect one hop later.
//
// Asserted on THE BODY THAT LEAVES THE PROCESS, captured by a recording upstream through the real
// gateway and the real adapter — because the drop happens in the gateway and the serialization in the
// adapter, so looking at either alone describes half the path.
func TestTheRequestDirectionOnTheShapeThisLayerRECEIVES(t *testing.T) {
	body := []byte(`{
	  "model": "wire-probe",
	  "messages": [
	    {"role": "user", "content": "extrae el total"},
	    {"role": "assistant", "content": null},
	    {"role": "assistant", "content": "", "reasoning_content": "no dije nada, y lo pensé"},
	    {"role": "user", "content": "sigue"}
	  ]
	}`)
	onTheWire := projectRequest(t, body)

	messages, _ := onTheWire["messages"].([]any)
	var roles []string
	for _, m := range messages {
		msg, _ := m.(map[string]any)
		role, _ := msg["role"].(string)
		roles = append(roles, role)
	}
	if want := []string{"user", "assistant", "user"}; !equalStrings(roles, want) {
		t.Fatalf("roles on the wire = %v, want %v — the turn with neither content nor tool_calls must "+
			"not leave the process, and the one that said the empty string must", roles, want)
	}

	assistant, _ := messages[1].(map[string]any)
	content, isString := assistant["content"].(string)
	if !isString || content != "" {
		t.Errorf("the surviving assistant content = %#v, want the empty STRING: null and \"\" are "+
			"different claims and the server answers 400 to one of them", assistant["content"])
	}
	if got, _ := assistant["reasoning_content"].(string); got != "no dije nada, y lo pensé" {
		t.Errorf("reasoning_content on the wire = %q, want it preserved — 7 of 120 turns failed without "+
			"it (llama.cpp#27720), and a transport that drops it reproduces that defect one hop later", got)
	}
}
