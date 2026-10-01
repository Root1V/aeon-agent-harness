// Package checkpoint is the durability seam agreed with the Synaptum framework team: the harness
// persists a run's journal and never decides anything about it.
//
// Two rules give the seam its shape, both from the tripartite agreement:
//
//   - Append is idempotent by (run_id, step_id, phase). A duplicate is a no-op and never an error,
//     because a caller running under at-least-once execution genuinely cannot tell a retry from a
//     first attempt — making it ask would push the hard part back across the seam.
//   - Load reconstructs a state that answers three questions and nothing else: was this step
//     completed, was it merely attempted, and where does the journal continue.
//
// What the seam deliberately does NOT do is decide whether a step should run again. That is the
// loop's call, and it needs knowledge the harness does not have — whether the step's effect is
// idempotent. See Attempted.
package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Phase says whether an entry was written before or after the step's effect.
//
// Having two phases is what makes a crash *during* an effect distinguishable from a crash before
// it. That distinction costs a second durable write per step, so the seam supports writing only
// PhaseCompleted — see RunState.Attempted for what the caller gives up by doing so.
type Phase string

const (
	// PhaseAttempted is written before the effect: intent recorded, outcome unknown.
	PhaseAttempted Phase = "attempted"
	// PhaseCompleted is written after the effect, with its result.
	PhaseCompleted Phase = "completed"
)

// Valid reports whether p is a phase this seam knows. Rejecting an unknown phase at the boundary
// matters more than it looks: phase is half of the idempotency key, so a typo would not fail —
// it would silently create a second entry for a step that already ran.
func (p Phase) Valid() bool {
	return p == PhaseAttempted || p == PhaseCompleted
}

// Entry is one journal append. Payload is opaque to the seam — the harness stores and returns it
// without interpreting it, which is what keeps "persists but never decides" true at the type level.
type Entry struct {
	RunID   string          `json:"run_id"`
	StepID  string          `json:"step_id"`
	Phase   Phase           `json:"phase"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Validate checks the identity fields the idempotency key is built from.
func (e Entry) Validate() error {
	if e.RunID == "" {
		return fmt.Errorf("checkpoint: run_id is required")
	}
	if e.StepID == "" {
		return fmt.Errorf("checkpoint: step_id is required")
	}
	if !e.Phase.Valid() {
		return fmt.Errorf("checkpoint: phase %q is not one of %q/%q", e.Phase, PhaseAttempted, PhaseCompleted)
	}
	if len(e.Payload) > 0 && !json.Valid(e.Payload) {
		return fmt.Errorf("checkpoint: payload is not valid JSON")
	}
	return nil
}

// Record is a stored Entry with the position and time the harness assigned it.
type Record struct {
	Entry
	Seq        int64     `json:"seq"`
	RecordedAt time.Time `json:"recorded_at"`
}

// AppendResult reports what an Append did. It carries no decision — only facts about the write.
type AppendResult struct {
	// Seq is the position of the entry in the run's journal: the newly assigned one, or the
	// existing one when Duplicate is true.
	Seq int64 `json:"seq"`
	// Duplicate is true when this identity was already journalled and nothing was written.
	Duplicate bool `json:"duplicate"`
	// PayloadDiverged is true when a duplicate arrived carrying a *different* payload than the
	// stored one. The stored payload wins — first write is the durable one — but the divergence is
	// reported rather than swallowed: two attempts of the same step producing different results is
	// real non-determinism, and a journal that hides it is worse than no journal.
	PayloadDiverged bool `json:"payload_diverged"`
}

// Checkpointer is the seam itself. Two methods, no third: anything that decides belongs on the
// other side of it.
type Checkpointer interface {
	Append(ctx context.Context, entry Entry) (AppendResult, error)
	Load(ctx context.Context, runID string) (*RunState, error)
}

// RunState is the reconstructed journal of one run.
type RunState struct {
	runID   string
	records []Record
	byStep  map[string]map[Phase]Record
	nextSeq int64
}

// NewRunState builds the queryable state from a run's records, in any order.
func NewRunState(runID string, records []Record) *RunState {
	sorted := append([]Record(nil), records...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })

	s := &RunState{runID: runID, records: sorted, byStep: map[string]map[Phase]Record{}}
	for _, rec := range sorted {
		if s.byStep[rec.StepID] == nil {
			s.byStep[rec.StepID] = map[Phase]Record{}
		}
		s.byStep[rec.StepID][rec.Phase] = rec
		if rec.Seq >= s.nextSeq {
			s.nextSeq = rec.Seq + 1
		}
	}
	return s
}

// RunID returns the run this state belongs to.
func (s *RunState) RunID() string { return s.runID }

// Completed reports a recorded result for stepID: the effect happened, so it must not be repeated,
// and the returned record carries the result the loop should continue from.
func (s *RunState) Completed(stepID string) (Record, bool) {
	rec, ok := s.byStep[stepID][PhaseCompleted]
	return rec, ok
}

// Attempted reports intent without a result: the process died somewhere between the two writes, so
// the effect *may* have happened. It is deliberately exclusive of Completed — a step that finished
// is not "attempted", it is done, and a caller asking "what do I do now" needs one answer, not two.
//
// A step whose caller only ever writes PhaseCompleted never reports true here. That is not a gap:
// after a crash such a step simply has no record at all and gets re-run, which is safe precisely
// when the effect is idempotent — which is the same condition under which skipping the second
// durable write was safe in the first place.
func (s *RunState) Attempted(stepID string) bool {
	phases := s.byStep[stepID]
	if phases == nil {
		return false
	}
	_, attempted := phases[PhaseAttempted]
	_, completed := phases[PhaseCompleted]
	return attempted && !completed
}

// NextSeq is the position the next append will take — where the journal continues.
func (s *RunState) NextSeq() int64 { return s.nextSeq }

// Records returns the journal in sequence order.
func (s *RunState) Records() []Record { return s.records }

// DurabilityLevel says how a run was journalled, which is the question an incident starts from and the
// one that used to require reading documentation (OBS-004).
type DurabilityLevel string

const (
	// DurabilityPerStep: every step recorded intent before its effect, so for ANY step that crashed we
	// can tell whether the effect may have landed.
	DurabilityPerStep DurabilityLevel = "per_step"
	// DurabilityPerActivity: no step recorded intent, so a crashed step has no record at all and gets
	// re-run. Safe exactly when every effect is idempotent, and silent when one is not.
	DurabilityPerActivity DurabilityLevel = "per_activity"
	// DurabilityMixed: some steps recorded intent and some did not.
	//
	// This is the EXPECTED state, not a warning, and saying so matters — A-07 settled on writing two
	// phases only for steps declared non-idempotent, so a healthy run is normally mixed. Reporting it as
	// an anomaly would teach whoever reads it to ignore the field.
	DurabilityMixed DurabilityLevel = "mixed"
	// DurabilityUnknown: nothing was journalled, so there is no evidence either way. Not "per_activity":
	// a run that recorded nothing and a run that deliberately recorded one phase per step are different
	// facts, and only one of them is a decision.
	DurabilityUnknown DurabilityLevel = "unknown"
)

// Durability is the journal's own answer to how durably a run was recorded.
//
// DERIVED FROM THE JOURNAL, never declared. That is the whole design: during an incident the useful
// fact is what actually got written, not what a config said should be. A declared level can be stale,
// wrong, or describe a deployment that was redeployed since; the records cannot.
type Durability struct {
	Level DurabilityLevel `json:"level"`
	// Steps is how many distinct steps the journal knows about.
	Steps int `json:"steps"`
	// StepsWithIntent is how many of them recorded intent before their effect. The ratio is the
	// actionable part: it says for how much of this run a crash is diagnosable.
	StepsWithIntent int `json:"steps_with_intent"`
}

// Durability computes the level from the records themselves.
func (s *RunState) Durability() Durability {
	d := Durability{Level: DurabilityUnknown, Steps: len(s.byStep)}
	for _, phases := range s.byStep {
		if _, ok := phases[PhaseAttempted]; ok {
			d.StepsWithIntent++
		}
	}
	switch {
	case d.Steps == 0:
		d.Level = DurabilityUnknown
	case d.StepsWithIntent == d.Steps:
		d.Level = DurabilityPerStep
	case d.StepsWithIntent == 0:
		d.Level = DurabilityPerActivity
	default:
		d.Level = DurabilityMixed
	}
	return d
}

// Outcome is WHY a step reached PhaseCompleted (INT-011).
//
// A denial is a completed step, not an absent one, and that is the whole point. Before this, a step
// denied BEFORE executing looked exactly like a step that was attempted and whose result nobody knows:
// both had no `completed` record. So a run suspended waiting for a person could not be resumed, because
// resuming means knowing which steps are still open, and "denied" was indistinguishable from "unknown".
//
// Found by Synaptum when they persisted their journal for real. It only appears once durability is,
// which is why it survived every in-memory test.
type Outcome string

const (
	// OutcomeResult is the ordinary case: the effect ran and the payload carries what it produced. It is
	// the DEFAULT for a completed record with no outcome field, so every record written before INT-011
	// keeps meaning exactly what it meant.
	OutcomeResult Outcome = "result"
	// OutcomeDeniedByPolicy: the Tool Gateway refused before the effect. Cedar is default-deny, so this
	// is also what an unknown tool or an unlisted principal produces.
	OutcomeDeniedByPolicy Outcome = "denied_by_policy"
	// OutcomeApprovalGranted: a person allowed the step. Written by the HARNESS, not the loop — a person
	// decides when the loop is not running, and only the harness knows it happened.
	OutcomeApprovalGranted Outcome = "approval_granted"
	// OutcomeApprovalDenied: a person refused it.
	OutcomeApprovalDenied Outcome = "approval_denied"
	// OutcomeApprovalExpired: nobody decided in time. Distinct from a denial on purpose — a denial is a
	// decision and an expiry is its absence, and they call for different things: the first ends the step,
	// the second can legitimately be asked again.
	OutcomeApprovalExpired Outcome = "approval_expired"
)

// Denied reports whether this outcome means the effect did NOT happen.
//
// A helper rather than a comparison at each call site, because the set will grow and a caller that
// enumerated it would keep passing while quietly treating a new denial kind as a success.
func (o Outcome) Denied() bool {
	switch o {
	case OutcomeDeniedByPolicy, OutcomeApprovalDenied, OutcomeApprovalExpired:
		return true
	default:
		return false
	}
}

// outcomeEnvelope is the shape an outcome travels in inside a record's payload.
type outcomeEnvelope struct {
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`
	// DecidedBy is WHO decided, for the outcomes a principal decides rather than a run reaching an end
	// (SEC-005). Omitted and not emptied: a record written before SEC-005 has no actor, and an empty
	// string here would read as a decision taken by a principal whose name happens to be blank. See
	// ApprovalActor for the three-state read.
	DecidedBy string `json:"decided_by,omitempty"`
	// Result is the ordinary payload, kept nested so an outcome can never be mistaken for it. A loop
	// reading `result` on a denied step finds nothing rather than finding something that looks usable.
	Result json.RawMessage `json:"result,omitempty"`
}

// OutcomePayload builds the payload for a completed record with an explicit outcome.
func OutcomePayload(outcome Outcome, reason string, result json.RawMessage) (json.RawMessage, error) {
	return OutcomePayloadDecidedBy(outcome, reason, "", result)
}

// OutcomePayloadDecidedBy is OutcomePayload plus the principal who decided (SEC-005).
//
// A SEPARATE CONSTRUCTOR rather than a fifth argument on the existing one, because most outcomes have
// no decider: a step that ran and returned was not decided by anybody, and giving every call site an
// actor argument invites passing something plausible there — the agent, the service, the run — which
// would put four kinds of thing in a field whose only question is "which person or process said yes".
func OutcomePayloadDecidedBy(outcome Outcome, reason, decidedBy string, result json.RawMessage) (json.RawMessage, error) {
	raw, err := json.Marshal(outcomeEnvelope{Outcome: outcome, Reason: reason, DecidedBy: decidedBy, Result: result})
	if err != nil {
		return nil, fmt.Errorf("checkpoint: encoding outcome: %w", err)
	}
	return raw, nil
}

// StepOutcome reports how a step concluded, and whether it concluded at all.
//
// The three answers a resuming loop needs, and they have to stay three: concluded-with-an-outcome,
// attempted-and-unknown, and never-seen. Collapsing the last two is what made a denied step
// unresumable in the first place.
func (s *RunState) StepOutcome(stepID string) (Outcome, string, bool) {
	rec, ok := s.Completed(stepID)
	if !ok {
		return "", "", false
	}
	if len(rec.Payload) == 0 {
		return OutcomeResult, "", true
	}
	var env outcomeEnvelope
	if err := json.Unmarshal(rec.Payload, &env); err != nil || env.Outcome == "" {
		// A payload that is not an envelope is a pre-INT-011 result, and reading it as one keeps every
		// existing journal meaning what it meant. Guessing a denial from an unparseable payload would be
		// far worse than assuming the ordinary case: it would strand a step that really did run.
		return OutcomeResult, "", true
	}
	return env.Outcome, env.Reason, true
}

// ApprovalActor returns who decided a step's outcome, and whether anybody is recorded at all.
//
// THREE STATES, like every other read in this file: a named decider, a record that names none, and no
// record. The middle one is not a gap to be filled with a guess — it is every approval taken before
// SEC-005, and reporting those as decided by "unknown" would invent a principal; reporting them as
// decided by nobody would claim the step was never approved. The caller gets the distinction and
// decides what to say about it.
func (s *RunState) ApprovalActor(stepID string) (string, bool) {
	rec, ok := s.Completed(stepID)
	if !ok || len(rec.Payload) == 0 {
		return "", false
	}
	var env outcomeEnvelope
	if err := json.Unmarshal(rec.Payload, &env); err != nil {
		return "", false
	}
	return env.DecidedBy, env.DecidedBy != ""
}

// ApprovalStep is the journal step_id an approval decision is recorded under (INT-011).
//
// Keyed by the APPROVAL and not by the node, which is the part worth agreeing on across the seam: two
// decisions about the same approval are the same fact and must collapse to one record, while a second
// approval on the same node — a retried step asking again — is a different fact and must not be
// swallowed by the first. Using node_id would have collapsed the second case into the first.
//
// A function rather than a documented string format, so the three teams derive the id from the same
// code path instead of each writing the prefix by hand.
func ApprovalStep(approvalID string) string { return "approval:" + approvalID }

// OutcomeForApproval maps a person's decision to the outcome recorded for it.
func OutcomeForApproval(approved bool) Outcome {
	if approved {
		return OutcomeApprovalGranted
	}
	return OutcomeApprovalDenied
}
