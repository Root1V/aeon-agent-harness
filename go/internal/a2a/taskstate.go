package a2a

import (
	sdka2a "github.com/a2aproject/a2a-go/a2a"
)

// terminalStates is a CLOSED set, and its closedness is the whole design (A2A-002, constraint (a)
// agreed with Synaptum on 2026-09-20).
//
// Classification asks "is this one of the states we KNOW ends a task", never "is this one of the states
// we know does not". The difference only shows up in the future: A2A v1.0.1 has an extension mechanism,
// so new states will appear, and a classifier built the other way round would treat every one of them
// as terminal. That failure is silent — the parent stops waiting and the remote's real answer is lost,
// with no error anywhere. Built this way, a new state is merely not-yet-finished, which costs a poll.
//
// Written down before the code that uses it existed, because this is exactly the rule a proxy quietly
// breaks while being optimised: "unknown means keep waiting" looks like a missing case to anyone
// tidying up a switch statement.
var terminalStates = map[sdka2a.TaskState]bool{
	sdka2a.TaskStateCompleted: true,
	sdka2a.TaskStateFailed:    true,
	sdka2a.TaskStateCanceled:  true,
	sdka2a.TaskStateRejected:  true,
}

// pausedForHuman are the states that look like an answer and are not (constraint (b)).
//
// input-required and auth-required are an APPROVAL ON THE OTHER SIDE OF THE NETWORK. Relaying them as
// terminal would make the parent continue with a response the remote never gave — the local analogue of
// treating our own PAUSED_FOR_APPROVAL as SUCCEEDED. They are not terminal above; this set exists so
// the proxy can say WHY a task is still open, since "waiting for a person over there" and "still
// working" call for different things from whoever is watching.
var pausedForHuman = map[sdka2a.TaskState]bool{
	sdka2a.TaskStateInputRequired: true,
	sdka2a.TaskStateAuthRequired:  true,
}

// StateClassification is what the proxy concluded about a remote task's state.
type StateClassification struct {
	// State is the string the remote sent, VERBATIM — including one we do not know. Kept as sent so a
	// record of an unknown state stays recognisable instead of claiming we understood it.
	State sdka2a.TaskState `json:"state"`
	// Terminal is true only for a state in the closed set above.
	Terminal bool `json:"terminal"`
	// PausedForHuman marks input-required/auth-required: open, and open for a reason that will not
	// resolve on its own.
	PausedForHuman bool `json:"paused_for_human,omitempty"`
	// Recognised is false for a state this build has never heard of. It travels alongside Terminal
	// instead of being folded into it, because "not finished" and "we do not know what this is" are
	// different facts and only the second is worth telling an operator about.
	Recognised bool `json:"recognised"`
}

// knownStates is every state this build recognises. Used only to populate Recognised — never to decide
// Terminal, because deciding from "not in the known list" is the inversion this file exists to avoid.
var knownStates = map[sdka2a.TaskState]bool{
	sdka2a.TaskStateSubmitted:     true,
	sdka2a.TaskStateWorking:       true,
	sdka2a.TaskStateInputRequired: true,
	sdka2a.TaskStateAuthRequired:  true,
	sdka2a.TaskStateCompleted:     true,
	sdka2a.TaskStateFailed:        true,
	sdka2a.TaskStateCanceled:      true,
	sdka2a.TaskStateRejected:      true,
	sdka2a.TaskStateUnknown:       true,
}

// ClassifyState decides whether a remote task has finished.
//
// An unrecognised state — and the SDK's own TaskStateUnknown, and an absent one — is reported as still
// working. Constraint (a): "un estado A2A desconocido se trata como working y nunca como terminal".
func ClassifyState(state sdka2a.TaskState) StateClassification {
	return StateClassification{
		State:          state,
		Terminal:       terminalStates[state],
		PausedForHuman: pausedForHuman[state],
		Recognised:     knownStates[state],
	}
}

// EffectiveState is the state the proxy reports upward for a non-terminal task.
//
// A state we do not recognise is reported as `working`, which is the agreed behaviour and also the only
// one a caller can act on: a caller handed a string it has never seen has no defined reaction, and the
// most likely one is to stop. The verbatim state stays in StateClassification.State and in the ledger,
// so nothing is lost — it is just not put where a decision is made.
func EffectiveState(c StateClassification) sdka2a.TaskState {
	if c.Terminal || c.Recognised {
		return c.State
	}
	return sdka2a.TaskStateWorking
}
