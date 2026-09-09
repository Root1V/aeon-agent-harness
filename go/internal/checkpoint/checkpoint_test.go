package checkpoint

import (
	"encoding/json"
	"testing"
	"time"
)

func rec(stepID string, phase Phase, seq int64, payload string) Record {
	r := Record{
		Entry:      Entry{RunID: "run-1", StepID: stepID, Phase: phase},
		Seq:        seq,
		RecordedAt: time.Now(),
	}
	if payload != "" {
		r.Payload = json.RawMessage(payload)
	}
	return r
}

// TestRunStateAnswersTheThreeReadings covers the part of INT-009 that needs no database: given a
// journal, what does the loop learn from it. The three cases below are the three ways a caller can
// use the two phases — the open question this seam leaves to Synaptum — so all three have to work.
func TestRunStateAnswersTheThreeReadings(t *testing.T) {
	t.Run("a step journalled in both phases is completed, not attempted", func(t *testing.T) {
		s := NewRunState("run-1", []Record{
			rec("s1", PhaseAttempted, 0, ""),
			rec("s1", PhaseCompleted, 1, `{"ok":true}`),
		})
		if s.Attempted("s1") {
			t.Error("a finished step must not also report as attempted — the caller needs one answer, not two")
		}
		got, ok := s.Completed("s1")
		if !ok {
			t.Fatal("expected s1 to report completed")
		}
		if string(got.Payload) != `{"ok":true}` {
			t.Errorf("completed payload = %s, want the recorded result", got.Payload)
		}
	})

	t.Run("a step journalled only as attempted is the crash-in-the-middle case", func(t *testing.T) {
		s := NewRunState("run-1", []Record{rec("s1", PhaseAttempted, 0, "")})
		if !s.Attempted("s1") {
			t.Error("expected s1 to report attempted — the effect may have happened")
		}
		if _, ok := s.Completed("s1"); ok {
			t.Error("expected s1 not to report completed")
		}
	})

	t.Run("a step journalled only as completed reports neither once absent", func(t *testing.T) {
		// This is the cheap reading: one durable write per step, no attempted phase. After a crash
		// mid-effect the step has no record at all, so the caller re-runs it — safe exactly when the
		// effect is idempotent, which is the same condition that made skipping the write safe.
		s := NewRunState("run-1", []Record{rec("s1", PhaseCompleted, 0, `1`)})
		if s.Attempted("s1") {
			t.Error("a completed-only step must not report attempted")
		}
		if s.Attempted("s2") || func() bool { _, ok := s.Completed("s2"); return ok }() {
			t.Error("an unjournalled step must report neither")
		}
	})

	t.Run("next_seq continues the journal and survives out-of-order input", func(t *testing.T) {
		s := NewRunState("run-1", []Record{
			rec("s3", PhaseCompleted, 2, ""),
			rec("s1", PhaseCompleted, 0, ""),
			rec("s2", PhaseCompleted, 1, ""),
		})
		if s.NextSeq() != 3 {
			t.Errorf("NextSeq() = %d, want 3", s.NextSeq())
		}
		got := s.Records()
		if len(got) != 3 || got[0].StepID != "s1" || got[2].StepID != "s3" {
			t.Errorf("records are not in sequence order: %+v", got)
		}
	})

	t.Run("an empty journal starts at zero", func(t *testing.T) {
		if s := NewRunState("run-1", nil); s.NextSeq() != 0 {
			t.Errorf("NextSeq() on an empty run = %d, want 0", s.NextSeq())
		}
	})
}

func TestEntryValidation(t *testing.T) {
	cases := []struct {
		name    string
		entry   Entry
		wantErr bool
	}{
		{"valid", Entry{RunID: "r", StepID: "s", Phase: PhaseCompleted}, false},
		{"missing run", Entry{StepID: "s", Phase: PhaseCompleted}, true},
		{"missing step", Entry{RunID: "r", Phase: PhaseCompleted}, true},
		// phase is half of the idempotency key: an unknown value would not fail loudly, it would
		// quietly journal a second entry for a step that already ran.
		{"unknown phase", Entry{RunID: "r", StepID: "s", Phase: "done"}, true},
		{"empty phase", Entry{RunID: "r", StepID: "s"}, true},
		{"payload that is not JSON", Entry{RunID: "r", StepID: "s", Phase: PhaseCompleted, Payload: json.RawMessage(`{`)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.entry.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
