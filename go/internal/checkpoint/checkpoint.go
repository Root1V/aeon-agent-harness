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
