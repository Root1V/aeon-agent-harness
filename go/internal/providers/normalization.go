package providers

import (
	"encoding/json"
	"errors"
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

// ToolCall is one call the model asked for, with its arguments DECODED — and, when they could not be
// decoded, with the wire string and the reason, so the turn still travels.
//
// Arguments is a map and never the JSON string the wire carries. The contract is explicit: the loop
// should not have to guess what it received. And a string that does not parse does NOT become an empty
// object — a silent `{}` would run the tool with no arguments, which is a different call that nobody
// asked for and which no error would mark.
//
// THREE STATES AND NOT TWO (MDL-023, asked for by Veritium in VRT-AEON-008). `arguments` is an object
// when it decoded, `{}` when the tool genuinely takes none, and **null** when the string did not parse —
// never `{}` for that last case. Until now the third state did not exist: the adapter returned an error,
// the gateway counted the candidate as failed, and `/v1/chat/completions` answered 502. That threw away
// a turn the model had already produced and been paid for. A malformed-arguments response is a model
// outcome, not a transport failure, and on the OpenAI wire `arguments` is a STRING precisely so that
// whoever executes decides what to do with one that does not parse. We do not execute here: this is a
// gateway. So the decision travels with the data instead of being made on the caller's behalf.
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	// RawArguments is the wire string EXACTLY as the provider sent it, for the dialects that send one.
	// Kept rather than re-encoded from Arguments: re-encoding reorders keys and normalises numbers, and
	// for a string that never parsed there is nothing to re-encode at all.
	RawArguments string `json:"raw_arguments,omitempty"`
	// ArgumentsError is why RawArguments did not decode, empty when it did. It names the failure and
	// never the bytes — see describeJSONFailure.
	ArgumentsError string `json:"arguments_error,omitempty"`
}

// ToolCallFromWire builds a ToolCall from the dialect's argument string and NEVER fails.
//
// The one constructor every adapter uses, so that "a malformed string is carried, not dropped" is a
// property of the type rather than of each adapter remembering. An adapter that decoded on its own and
// returned an error is what produced the 502 in VRT-AEON-008 — three adapters did it, three times,
// each with its own wording.
func ToolCallFromWire(id, name, raw string) ToolCall {
	tc := ToolCall{ID: id, Name: name, RawArguments: raw}
	args, err := DecodeToolArguments(raw)
	if err != nil {
		// Arguments stays nil, which marshals to `null`: the state that says "did not decode" and that
		// no consumer can mistake for an empty object.
		tc.ArgumentsError = err.Error()
		return tc
	}
	tc.Arguments = args
	return tc
}

// DecodeToolArguments parses the wire's argument string. STILL AN ERROR for whoever executes: this is
// the decoder an executor calls, and for it a string that does not parse is a failure. What changed in
// MDL-023 is who calls it — a transport records the error and forwards the turn (ToolCallFromWire),
// while a loop that is about to run the tool must not.
//
// An empty string decodes to an empty map, which is the one case where `{}` is right: a tool with no
// parameters genuinely sends nothing.
func DecodeToolArguments(raw string) (map[string]any, error) {
	if raw == "" {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, fmt.Errorf("providers: tool call arguments are not a JSON object (%s) — refusing to "+
			"substitute an empty object, which would run the tool with no arguments and report success",
			describeJSONFailure(raw, err))
	}
	return args, nil
}

// describeJSONFailure says WHAT failed and WHERE, and never what the bytes were.
//
// THE SECOND HALF OF VRT-AEON-008, and it is a personal-data defect and not cosmetics. The error used to
// carry `%q` of the whole argument string, so a truncated extraction of a real payslip put the holder's
// name into a 502 body, into the caller's logs and into both sides' traces. Their sentence is the one
// that settles it: *«un nombre no se puede detectar con una expresión regular»* — a redactor downstream
// cannot fix this, so the text must never contain the content in the first place.
//
// The wrapped error is NOT included either, deliberately: encoding/json's syntax messages quote the
// offending byte ("invalid character 'e' looking for beginning of value"), and one byte of a name is
// still a byte of a name. What is kept is what an operator acts on — how long it was, where it broke,
// and which of the two failures it was.
func describeJSONFailure(raw string, err error) string {
	var syntax *json.SyntaxError
	var mismatch *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return fmt.Sprintf("%d bytes, malformed JSON at byte %d", len(raw), syntax.Offset)
	case errors.As(err, &mismatch):
		// mismatch.Value is a JSON type name ("string", "number", "array"), never a value.
		return fmt.Sprintf("%d bytes, well-formed JSON but a %s and not an object, at byte %d",
			len(raw), mismatch.Value, mismatch.Offset)
	default:
		return fmt.Sprintf("%d bytes, malformed JSON", len(raw))
	}
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
		// THE WIRE STRING IS DROPPED FROM THIS VIEW, and it is the only field that is (MDL-023).
		//
		// This is the decoded view: `arguments` is the object, or null when it did not parse, and
		// `arguments_error` says why. The string itself belongs in the OpenAI-shaped `tool_calls`, whose
		// format defines that field as a string — and it is already there, in the same response.
		//
		// Carrying it in both places was caught by the test for VRT-58: a truncated extraction has
		// personal data IN the arguments, so every extra field holding a copy is one more place a
		// downstream log can print it. One copy, in the field a caller reads to execute.
		tc.RawArguments = ""
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
