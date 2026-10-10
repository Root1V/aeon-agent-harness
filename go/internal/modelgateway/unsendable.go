package modelgateway

// VRT-SYN-004: an assistant turn that nothing on the chat wire can carry.
//
// WHAT BREAKS WITHOUT THIS, measured against the real prometheus deployment on 2026-10-10: a
// rendered context whose history holds an assistant turn with no `content` and no `tool_calls` comes
// back `400 Assistant message must contain either 'content' or 'tool_calls'!`, which this gateway
// then reports as "all candidates failed: HTTP 400" — unactionable for the caller, and fatal to the
// run. It is reached whenever a reasoning model answers with reasoning and nothing else and the
// caller replays that turn in its history: intermittent, and likelier the longer the run.
//
// WHY HERE AND NOT IN THE ADAPTERS. Three places put messages on the wire — prometheus_inference's
// typed mapping, and openai_compatible's Decide and DecideStream, which marshal the rendered context
// verbatim — so fixing it there is three implementations of one rule, which is the shape that just
// cost INT-014 a dropped field in two hand-written copies. Every API door reaches a provider through
// this package (measured: no caller invokes an adapter's Decide directly), both of its call sites
// build the provider input in one place each, and both have the span in scope to report on. One rule,
// and any adapter added later inherits it.
//
// DROPPED AND NOT REPAIRED, with all three options measured:
//
//	assistant, reasoning_content only    -> 400  the defect
//	assistant, content: ""               -> 200  accepted, but asserts the assistant said ""
//	assistant, no content but tool_calls -> 200  the ordinary tool-calling turn, left alone
//	the turn removed entirely            -> 200  so dropping is a valid repair
//
// `content: ""` would keep the turn but claim the assistant said the empty string, which it did not.
// Refusing the request is honest and loses a run over an envelope carrying nothing. Dropping removes
// nothing transmissible — the reasoning was never going on the wire. What made dropping objectionable
// is being silent about it, so the count is reported on the span and in the result: a gateway that
// narrows a caller's request without saying so is the exact defect VRT-AEON-003 was raised about.
//
// ONLY the assistant role. A user or tool turn with no content is the caller's own bug, and removing
// it quietly would hide it.
func dropUnsendableAssistantTurns(input map[string]any) int {
	raw, ok := input["messages"].([]any)
	if !ok {
		return 0
	}

	kept := make([]any, 0, len(raw))
	dropped := 0
	for _, rm := range raw {
		m, ok := rm.(map[string]any)
		if !ok || !unsendableAssistantTurn(m) {
			kept = append(kept, rm)
			continue
		}
		dropped++
	}
	if dropped == 0 {
		// The caller's slice is left exactly as it was when there is nothing to do, so a request that
		// needed no repair is not even reallocated.
		return 0
	}
	// A NEW slice, assigned into the copy this gateway made: `input` is a fresh map at both call
	// sites, but its `messages` value is the caller's own slice, and writing through it would edit
	// the request the caller still holds.
	input["messages"] = kept
	return dropped
}

// unsendableAssistantTurn decides it on the wire shape, where the distinction that matters is
// between a `content` that is absent-or-null and one that is an explicit empty string. The second is
// sendable and measured to be accepted; collapsing the two would turn a working request into a
// dropped turn.
func unsendableAssistantTurn(m map[string]any) bool {
	if role, _ := m["role"].(string); role != "assistant" {
		return false
	}
	if content, present := m["content"]; present && content != nil {
		return false
	}
	calls, _ := m["tool_calls"].([]any)
	return len(calls) == 0
}
