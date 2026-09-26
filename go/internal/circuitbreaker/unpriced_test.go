package circuitbreaker

import (
	"strings"
	"testing"
)

// TestUnpricedRunsDoNotSilenceTheCostBreaker is OBS-009's acceptance test.
//
// A5's cost check quarantines a version whose average spend per run is anomalous.
// recordOutcomeRequest.CostUSD was a float64 with omitempty, so a run whose cost nobody computed —
// a compute_based provider, a model with no configured rate — arrived as 0 and was averaged in. The
// average fell, and the more unpriced spend a version accumulated the safer it looked. That is
// fail-open in the one control meant to catch a runaway agent.
//
// It bites exactly where this platform runs: prometheus_inference is compute_based, so today EVERY
// local call is unpriced. Found on 2026-09-23 after Axonium reported Prometheus had
// `sum(row["cost_usd"] or 0.0)` on their own billing page — every row honestly showing a dash, the
// total saying USD 0.00. Theirs misinformed a customer; ours disabled a safety control.
func TestUnpricedRunsDoNotSilenceTheCostBreaker(t *testing.T) {
	thresholds := func() Thresholds {
		return Thresholds{WindowSize: 20, MinSamples: 5, MaxFailureRate: 1.0, MaxAvgCostUSD: 1.0}
	}

	t.Run("unpriced runs beside expensive ones do not stop the trip", func(t *testing.T) {
		// This is the measurement that opened OBS-009, as a test. Identical real spend of $20 in
		// both halves; before the fix the second did not trip. The interleaving matters: the breaker
		// latches, so appending the unpriced runs after the trip would prove nothing.
		expensive := New(thresholds())
		var trippedWithoutUnpriced bool
		for i := 0; i < 10; i++ {
			if v := expensive.RecordOutcome("a", "1", Observation{Success: true, CostUSD: usd(2.0)}); v.Tripped {
				trippedWithoutUnpriced = true
				break
			}
		}
		if !trippedWithoutUnpriced {
			t.Fatal("ten runs at $2.00 against a $1.00 threshold did not trip — the control is broken before we even get to the unpriced case")
		}

		mixed := New(thresholds())
		var verdict Verdict
		// Stop at the FIRST tripping verdict. The breaker latches, so every later call answers
		// "already quarantined" and keeping the last one would assert against that instead of
		// against the reason the trip actually gave.
		for i := 0; i < 10 && !verdict.Tripped; i++ {
			if v := mixed.RecordOutcome("b", "1", Observation{Success: true}); v.Tripped { // cost unknown
				verdict = v
				break
			}
			if v := mixed.RecordOutcome("b", "1", Observation{Success: true, CostUSD: usd(2.0)}); v.Tripped {
				verdict = v
			}
		}
		if !verdict.Tripped {
			t.Fatal("the same $20 of real spend did not trip once unpriced runs were interleaved — unknown cost is being averaged in as zero")
		}
		if !strings.Contains(verdict.Reason, "priced run(s)") {
			t.Errorf("reason = %q, want it to say the average covers the priced runs only", verdict.Reason)
		}
		if !strings.Contains(verdict.Reason, "no known cost") {
			t.Errorf("reason = %q, want it to disclose how many runs the figure does not cover", verdict.Reason)
		}
	})

	t.Run("a measured zero still counts, and still holds the average down", func(t *testing.T) {
		// The half that makes the first subtest mean something. If nil and 0 were treated alike in the
		// other direction — ignoring real zeros — a genuinely free model would stop diluting the
		// average and the breaker would trip on spend that is not anomalous at all.
		b := New(thresholds())
		var verdict Verdict
		for i := 0; i < 10; i++ {
			b.RecordOutcome("c", "1", Observation{Success: true, CostUSD: usd(0)}) // measured, and free
			verdict = b.RecordOutcome("c", "1", Observation{Success: true, CostUSD: usd(2.0)})
		}
		if verdict.Tripped {
			t.Fatalf("tripped at a real average of $1.00 against a $1.00 threshold: %q — a measured zero is data and must dilute", verdict.Reason)
		}
	})

	t.Run("a window with no priced run at all does not trip", func(t *testing.T) {
		// Nothing was measured, so there is no average to compare. Tripping here would quarantine a
		// version for spending an amount nobody knows — the mirror failure of the original defect,
		// and the one a careless fix introduces.
		b := New(thresholds())
		var verdict Verdict
		for i := 0; i < 10; i++ {
			verdict = b.RecordOutcome("d", "1", Observation{Success: true})
		}
		if verdict.Tripped {
			t.Fatalf("tripped with no priced run in the window: %q", verdict.Reason)
		}
	})

	t.Run("the failure-rate check is untouched by unknown cost", func(t *testing.T) {
		// The two thresholds are independent, and a run whose cost is unknown is still a run whose
		// success is known. Cost blindness must not make a failing version look healthy.
		b := New(Thresholds{WindowSize: 10, MinSamples: 5, MaxFailureRate: 0.5, MaxAvgCostUSD: 1.0})
		var verdict Verdict
		for i := 0; i < 6 && !verdict.Tripped; i++ {
			verdict = b.RecordOutcome("e", "1", Observation{Success: false}) // failing, cost unknown
		}
		if !verdict.Tripped {
			t.Fatal("six consecutive failures did not trip the failure-rate check")
		}
		if !strings.Contains(verdict.Reason, "failure rate") {
			t.Errorf("reason = %q, want the failure-rate reason", verdict.Reason)
		}
	})
}
