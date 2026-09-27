package providers

import (
	"encoding/json"
	"fmt"
)

// The typed vocabulary of the shared normalization contract (FND-004).
//
// See evals/contracts/normalizacion/spec.md, written by the Synaptum team under H1 = D: the
// normalization is SPECIFIED ONCE and implemented per language. These names are the contract's, not
// ours, and that is the point — what keeps the Go and Python implementations from drifting is not
// trust, it is both of them running the same fixtures against the same bodies.
const (
	// PartThinking is chain-of-thought. A SEPARATE part and never concatenated into text: the two are
	// different flows, and a consumer rendering a live answer would otherwise print the model's
	// deliberation as if it were the reply.
	PartThinking = "thinking"
	// PartText is the answer.
	PartText = "text"
	// PartToolCall is a call the model wants made.
	PartToolCall = "tool_call"
)

// ToolCall is one call the model asked for, with its arguments DECODED.
//
// Arguments is a map and never the JSON string the wire carries. The contract is explicit: the loop
// should not have to guess what it received. And a string that does not parse is an ERROR rather than an
// empty object — a silent `{}` would run the tool with no arguments, which is a different call that
// nobody asked for and which no error would mark.
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// DecodeToolArguments parses the wire's argument string.
//
// An empty string decodes to an empty map, which is the one case where `{}` is right: a tool with no
// parameters genuinely sends nothing. Anything else that fails to parse is returned as an error.
func DecodeToolArguments(raw string) (map[string]any, error) {
	if raw == "" {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, fmt.Errorf("providers: tool call arguments are not a JSON object (%q): %w — refusing to "+
			"substitute an empty object, which would run the tool with no arguments and report success", raw, err)
	}
	return args, nil
}

// ContentPart is one typed piece of an assistant message.
type ContentPart struct {
	Kind string `json:"kind"`
	// Text carries the content of a text or thinking part.
	Text string `json:"text,omitempty"`
	// Signature is the opaque blob some providers require returned intact on the next turn. Transported
	// without being interpreted, because interpreting it is how a transport starts depending on a format
	// it does not own.
	Signature string `json:"signature,omitempty"`
	// ToolCall is set on a tool_call part.
	ToolCall *ToolCall `json:"tool_call,omitempty"`
}

// ContentParts builds the typed parts of a message from what an adapter extracted.
//
// Order is thinking, then text, then tool calls, which is the order they are produced: reasoning
// arrives before any answer token.
//
// AN EMPTY TEXT PART IS NEVER EMITTED. The contract calls this out and the reason is concatenation: a
// consumer joining the text parts of a tool-call-only response would produce "" either way, but one that
// counts parts, or checks whether the model said anything, gets a different answer. An absent text part
// and an empty one are not the same message.
func ContentParts(reasoning, text string, toolCalls []ToolCall, signature string) []ContentPart {
	parts := make([]ContentPart, 0, 2+len(toolCalls))
	if reasoning != "" {
		parts = append(parts, ContentPart{Kind: PartThinking, Text: reasoning, Signature: signature})
	}
	if text != "" {
		parts = append(parts, ContentPart{Kind: PartText, Text: text})
	}
	for i := range toolCalls {
		tc := toolCalls[i]
		parts = append(parts, ContentPart{Kind: PartToolCall, ToolCall: &tc})
	}
	return parts
}

// DerivedUsage marks a Usage whose counters were computed rather than reported.
//
// `estimated` is the contract's third state and it is a *bool on Usage for the same reason every counter
// here is a pointer: nil says nothing about provenance, false says the provider REPORTED these, and true
// says we derived them. A plain bool would make every unreported usage look reported.
func DerivedUsage(u Usage) Usage {
	t := true
	u.Estimated = &t
	return u
}

// ReportedUsage marks a Usage that came from the provider's own usage object.
func ReportedUsage(u Usage) Usage {
	f := false
	u.Estimated = &f
	return u
}
