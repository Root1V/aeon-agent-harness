package checkpoint_test

import (
	"testing"

	"github.com/aeon-ai/aeon/go/internal/checkpoint"
)

// TestRunReportsDurabilityLevel is OBS-004's acceptance test.
//
// A-07 settled that two durable writes per step — intent, then result — are paid only for steps declared
// non-idempotent, because `attempted` only changes what you do precisely when repeating the effect is
// dangerous. That made the level a per-run, per-step FACT rather than a deployment setting, and until
// now the only way to learn it was to read A-07. An incident responder at 3am does not read A-07.
//
// The level is DERIVED FROM THE JOURNAL and never declared, and that is the design rather than an
// implementation detail: a declared level can be stale, or wrong, or describe a deployment that has been
// redeployed since. The records cannot. What happened is what got written.
func TestRunReportsDurabilityLevel(t *testing.T) {
	state := func(t *testing.T, entries ...checkpoint.Record) *checkpoint.RunState {
		t.Helper()
		return checkpoint.NewRunState("run-1", entries)
	}
	rec := func(step string, phase checkpoint.Phase, seq int64) checkpoint.Record {
		return checkpoint.Record{
			Entry: checkpoint.Entry{RunID: "run-1", StepID: step, Phase: phase},
			Seq:   seq,
		}
	}

	t.Run("every step recorded intent: per_step", func(t *testing.T) {
		d := state(t,
			rec("a", checkpoint.PhaseAttempted, 1), rec("a", checkpoint.PhaseCompleted, 2),
			rec("b", checkpoint.PhaseAttempted, 3), rec("b", checkpoint.PhaseCompleted, 4),
		).Durability()

		if d.Level != checkpoint.DurabilityPerStep {
			t.Errorf("Level = %q, want per_step", d.Level)
		}
		if d.Steps != 2 || d.StepsWithIntent != 2 {
			t.Errorf("steps=%d with intent=%d, want 2 and 2", d.Steps, d.StepsWithIntent)
		}
	})

	t.Run("no step recorded intent: per_activity", func(t *testing.T) {
		// Safe exactly when every effect is idempotent, and silent when one is not — which is why this
		// being READABLE matters. A crashed step here has no record at all and is simply re-run.
		d := state(t, rec("a", checkpoint.PhaseCompleted, 1), rec("b", checkpoint.PhaseCompleted, 2)).Durability()

		if d.Level != checkpoint.DurabilityPerActivity {
			t.Errorf("Level = %q, want per_activity", d.Level)
		}
		if d.StepsWithIntent != 0 {
			t.Errorf("StepsWithIntent = %d, want 0", d.StepsWithIntent)
		}
	})

	t.Run("mixed is the EXPECTED state, not an anomaly", func(t *testing.T) {
		// The subtest that keeps this field usable. Under A-07 a healthy run is normally mixed: the
		// non-idempotent steps pay two writes and the rest pay one. If mixed were reported as a warning,
		// whoever reads the field would learn to ignore it — and then it would be useless on the day it
		// says something.
		d := state(t,
			rec("dangerous", checkpoint.PhaseAttempted, 1), rec("dangerous", checkpoint.PhaseCompleted, 2),
			rec("idempotent", checkpoint.PhaseCompleted, 3),
		).Durability()

		if d.Level != checkpoint.DurabilityMixed {
			t.Errorf("Level = %q, want mixed", d.Level)
		}
		if d.Steps != 2 || d.StepsWithIntent != 1 {
			t.Errorf("steps=%d with intent=%d, want 2 and 1 — the ratio is the actionable part: it says for how much of the run a crash is diagnosable", d.Steps, d.StepsWithIntent)
		}
	})

	t.Run("an empty journal is unknown, NOT per_activity", func(t *testing.T) {
		// Three states rather than two, the same rule as every counter this phase touched. A run that
		// recorded nothing and a run that deliberately recorded one phase per step are different facts,
		// and only the second is a decision. Calling the first per_activity would report an absence of
		// evidence as a choice someone made.
		d := state(t).Durability()

		if d.Level != checkpoint.DurabilityUnknown {
			t.Errorf("Level = %q, want unknown", d.Level)
		}
		if d.Steps != 0 {
			t.Errorf("Steps = %d, want 0", d.Steps)
		}
	})

	t.Run("a step with intent but no result still counts as covered", func(t *testing.T) {
		// The crash case, which is the whole point of writing intent at all: the process died between the
		// two writes. Its durability coverage is not diminished by the crash — it is what MAKES the crash
		// diagnosable, and RunState.Attempted can answer for it.
		s := state(t, rec("a", checkpoint.PhaseAttempted, 1))
		d := s.Durability()

		if d.Level != checkpoint.DurabilityPerStep || d.StepsWithIntent != 1 {
			t.Errorf("Level=%q StepsWithIntent=%d, want per_step and 1", d.Level, d.StepsWithIntent)
		}
		if !s.Attempted("a") {
			t.Error("Attempted is false for a step with intent and no result — the two answers disagree about the same journal")
		}
	})
}
