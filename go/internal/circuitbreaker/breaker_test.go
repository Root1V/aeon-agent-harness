package circuitbreaker

import (
	"strings"
	"testing"
)

func TestRecordOutcomeDoesNotTripBelowMinSamples(t *testing.T) {
	b := New(Thresholds{WindowSize: 10, MinSamples: 5, MaxFailureRate: 0.5})
	for i := 0; i < 4; i++ {
		v := b.RecordOutcome("agent", "1.0.0", Observation{Success: false})
		if v.Tripped {
			t.Fatalf("call %d: tripped before MinSamples reached", i)
		}
	}
}

func TestRecordOutcomeTripsOnFailureRate(t *testing.T) {
	b := New(Thresholds{WindowSize: 10, MinSamples: 4, MaxFailureRate: 0.5})
	// 3 failures, 1 success: failure rate 0.75 > 0.5.
	b.RecordOutcome("agent", "1.0.0", Observation{Success: false})
	b.RecordOutcome("agent", "1.0.0", Observation{Success: false})
	b.RecordOutcome("agent", "1.0.0", Observation{Success: true})
	v := b.RecordOutcome("agent", "1.0.0", Observation{Success: false})
	if !v.Tripped {
		t.Fatal("expected the breaker to trip once failure rate exceeds the threshold")
	}
	if !strings.Contains(v.Reason, "failure rate") {
		t.Fatalf("unexpected reason: %q", v.Reason)
	}
	if !b.IsQuarantined("agent", "1.0.0") {
		t.Fatal("expected IsQuarantined = true after a trip")
	}
}

func TestRecordOutcomeDoesNotTripUnderThreshold(t *testing.T) {
	b := New(Thresholds{WindowSize: 10, MinSamples: 4, MaxFailureRate: 0.5})
	// 1 failure, 3 successes: failure rate 0.25 < 0.5.
	b.RecordOutcome("agent", "1.0.0", Observation{Success: true})
	b.RecordOutcome("agent", "1.0.0", Observation{Success: true})
	b.RecordOutcome("agent", "1.0.0", Observation{Success: true})
	v := b.RecordOutcome("agent", "1.0.0", Observation{Success: false})
	if v.Tripped {
		t.Fatalf("did not expect a trip at failure rate under threshold, got reason: %q", v.Reason)
	}
}

func TestRecordOutcomeTripsOnAnomalousCost(t *testing.T) {
	b := New(Thresholds{WindowSize: 10, MinSamples: 2, MaxFailureRate: 1.0, MaxAvgCostUSD: 1.0})
	b.RecordOutcome("agent", "1.0.0", Observation{Success: true, CostUSD: 0.5})
	v := b.RecordOutcome("agent", "1.0.0", Observation{Success: true, CostUSD: 5.0})
	if !v.Tripped {
		t.Fatal("expected the breaker to trip on anomalous average cost")
	}
	if !strings.Contains(v.Reason, "average cost") {
		t.Fatalf("unexpected reason: %q", v.Reason)
	}
}

func TestMaxAvgCostUSDZeroDisablesCostCheck(t *testing.T) {
	b := New(Thresholds{WindowSize: 10, MinSamples: 1, MaxFailureRate: 1.0, MaxAvgCostUSD: 0})
	v := b.RecordOutcome("agent", "1.0.0", Observation{Success: true, CostUSD: 1_000_000})
	if v.Tripped {
		t.Fatal("expected MaxAvgCostUSD=0 to disable the cost check entirely")
	}
}

func TestWindowIsBoundedToWindowSize(t *testing.T) {
	b := New(Thresholds{WindowSize: 3, MinSamples: 3, MaxFailureRate: 1.0}) // 1.0: never trips, isolates the bounding behavior
	for i := 0; i < 50; i++ {
		b.RecordOutcome("agent", "1.0.0", Observation{Success: i%2 == 0})
	}
	if got := len(b.windows[agentKey{"agent", "1.0.0"}]); got != 3 {
		t.Fatalf("window length = %d after 50 pushes, want bounded to WindowSize (3)", got)
	}
}

func TestOnceTrippedStaysTrippedUntilReset(t *testing.T) {
	b := New(Thresholds{WindowSize: 3, MinSamples: 2, MaxFailureRate: 0.5})
	b.RecordOutcome("agent", "1.0.0", Observation{Success: false})
	v := b.RecordOutcome("agent", "1.0.0", Observation{Success: false})
	if !v.Tripped {
		t.Fatal("expected a trip")
	}
	// Fresh successes after tripping must not "un-trip" it on their own.
	v2 := b.RecordOutcome("agent", "1.0.0", Observation{Success: true})
	if !v2.Tripped {
		t.Fatal("expected the breaker to stay tripped after fresh successes, until Reset")
	}
	b.Reset("agent", "1.0.0")
	if b.IsQuarantined("agent", "1.0.0") {
		t.Fatal("expected IsQuarantined = false after Reset")
	}
}

func TestWindowsAreIndependentPerAgentVersion(t *testing.T) {
	b := New(Thresholds{WindowSize: 10, MinSamples: 2, MaxFailureRate: 0.5})
	b.RecordOutcome("agent", "1.0.0", Observation{Success: false})
	b.RecordOutcome("agent", "1.0.0", Observation{Success: false})
	if b.IsQuarantined("agent", "2.0.0") {
		t.Fatal("expected a different version's window to be unaffected")
	}
}

func TestNewWithZeroValueThresholdsUsesDefaults(t *testing.T) {
	b := New(Thresholds{})
	if b.thresholds != DefaultThresholds {
		t.Fatalf("thresholds = %+v, want DefaultThresholds %+v", b.thresholds, DefaultThresholds)
	}
}
