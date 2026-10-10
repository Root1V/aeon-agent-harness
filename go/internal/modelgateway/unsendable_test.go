package modelgateway

import (
	"reflect"
	"testing"
)

// TestUnsendableAssistantTurnsAreDroppedAndCounted covers VRT-SYN-004's rule with the four shapes
// that were measured against the real deployment on 2026-10-10. The real-inference proof is
// TestReasoningOnlyAssistantTurnDoesNotBreakTheRun (make test-vrt-aeon-003); this one runs in CI,
// where there are no credentials, and pins the BOUNDARY — which is the part a future edit gets
// wrong.
func TestUnsendableAssistantTurnsAreDroppedAndCounted(t *testing.T) {
	user := map[string]any{"role": "user", "content": "hola"}

	cases := []struct {
		name    string
		message map[string]any
		dropped bool
		why     string
	}{{
		name:    "reasoning only",
		message: map[string]any{"role": "assistant", "reasoning_content": "pensando"},
		dropped: true,
		why:     "measured 400: nothing on the wire carries it once the reasoning is left out",
	}, {
		name:    "content absent entirely",
		message: map[string]any{"role": "assistant"},
		dropped: true,
		why:     "same shape as reasoning-only once the reasoning key is gone",
	}, {
		name:    "content is null",
		message: map[string]any{"role": "assistant", "content": nil},
		dropped: true,
		why:     "an explicit null is not a value the wire can carry either",
	}, {
		// THE BOUNDARY. Collapsing "" with nil would drop a turn the platform accepts — measured 200.
		// A turn that said nothing is not a turn that said the empty string.
		name:    "content is the empty string",
		message: map[string]any{"role": "assistant", "content": ""},
		dropped: false,
		why:     "measured 200: an explicit empty string is sendable",
	}, {
		name: "no content but tool_calls",
		message: map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "c1", "function": map[string]any{"name": "f", "arguments": "{}"}},
		}},
		dropped: false,
		why:     "the ordinary tool-calling turn, measured 200",
	}, {
		// Not ours to repair: a user turn with no content is the caller's bug, and removing it
		// quietly would hide it.
		name:    "a USER turn with no content is left alone",
		message: map[string]any{"role": "user"},
		dropped: false,
		why:     "only the assistant role is in scope",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := map[string]any{"messages": []any{user, tc.message, user}}
			got := dropUnsendableAssistantTurns(input)

			want := 0
			if tc.dropped {
				want = 1
			}
			if got != want {
				t.Fatalf("dropped = %d, want %d — %s", got, want, tc.why)
			}
			kept, _ := input["messages"].([]any)
			if len(kept) != 3-want {
				t.Fatalf("messages = %d, want %d", len(kept), 3-want)
			}
			if !tc.dropped && !reflect.DeepEqual(kept[1], tc.message) {
				t.Errorf("the kept turn was altered: %v", kept[1])
			}
		})
	}

	t.Run("the caller's own slice is not edited", func(t *testing.T) {
		original := []any{user, map[string]any{"role": "assistant"}, user}
		input := map[string]any{"messages": original}
		if got := dropUnsendableAssistantTurns(input); got != 1 {
			t.Fatalf("dropped = %d, want 1", got)
		}
		// The gateway copies the rendered-context MAP but not the slice inside it, so writing through
		// that slice would edit the request the caller still holds — a caller that retries would then
		// be retrying a conversation we rewrote behind its back.
		if len(original) != 3 {
			t.Fatalf("the caller's slice is now %d long: the repair wrote through it", len(original))
		}
		if _, isAssistantTurn := original[1].(map[string]any)["role"]; !isAssistantTurn {
			t.Error("the caller's second message was replaced")
		}
	})

	t.Run("a request needing no repair is left untouched", func(t *testing.T) {
		messages := []any{user, map[string]any{"role": "assistant", "content": "hola"}}
		input := map[string]any{"messages": messages}
		if got := dropUnsendableAssistantTurns(input); got != 0 {
			t.Fatalf("dropped = %d, want 0", got)
		}
		// Same slice, not a copy: nothing to do means nothing done, down to the allocation.
		if reflect.ValueOf(input["messages"]).Pointer() != reflect.ValueOf(messages).Pointer() {
			t.Error("the messages slice was reallocated for a request that needed no repair")
		}
	})

	t.Run("a context with no messages at all is not an error here", func(t *testing.T) {
		if got := dropUnsendableAssistantTurns(map[string]any{}); got != 0 {
			t.Fatalf("dropped = %d, want 0", got)
		}
		// Refusing an empty context is the adapter's job and it already does it ("no messages in the
		// rendered context"); duplicating that judgement here would give two places that decide it.
	})
}
