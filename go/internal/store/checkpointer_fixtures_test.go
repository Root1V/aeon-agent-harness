package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/checkpoint"
)

// The golden fixtures are the contract shared with Synaptum and Axonium, not an Aeon test asset —
// see evals/contracts/README.md for where the canonical copy lives and why this one exists.
type seamFixtureFile struct {
	Contract string            `json:"contract"`
	Version  string            `json:"version"`
	Cases    []seamFixtureCase `json:"cases"`
}

type seamFixtureCase struct {
	Name       string          `json:"name"`
	Why        string          `json:"why"`
	Operations []seamFixtureOp `json:"operations"`
}

type seamFixtureOp struct {
	Op          string          `json:"op"`
	StepID      string          `json:"step_id"`
	Phase       string          `json:"phase"`
	Payload     json.RawMessage `json:"payload"`
	Expect      json.RawMessage `json:"expect"`
	ExpectError bool            `json:"expect_error"`
}

func contractsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "evals", "contracts")
}

// TestCheckpointerPassesSharedSeamFixtures runs the golden corpus published to the other two teams
// against this implementation. Publishing a contract and calling ours the reference implementation
// is a claim; this is what turns it into a checked fact, and it is the reason the fixtures describe
// observable results rather than internals — Synaptum's Python implementation has to be able to run
// the same cases without sharing a line of code with us.
func TestCheckpointerPassesSharedSeamFixtures(t *testing.T) {
	cp := newTestCheckpointer(t)
	ctx := context.Background()

	path := filepath.Join(contractsDir(t), "costura-durabilidad", "fixtures", "dedup-por-identidad.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading shared fixtures: %v", err)
	}
	var file seamFixtureFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parsing shared fixtures: %v", err)
	}
	if len(file.Cases) == 0 {
		t.Fatal("the shared fixture file has no cases")
	}
	t.Logf("running %d shared cases from contract %s %s", len(file.Cases), file.Contract, file.Version)

	for i, tc := range file.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			// Every case starts on a clean run, which is what lets them be read (and reordered)
			// independently — the corpus is a contract, not a script with hidden shared state.
			runID := newCheckpointRunID(fmt.Sprintf("fixture-%d", i))

			for j, op := range tc.Operations {
				switch op.Op {
				case "append":
					res, err := cp.Append(ctx, checkpoint.Entry{
						RunID: runID, StepID: op.StepID, Phase: checkpoint.Phase(op.Phase), Payload: op.Payload,
					})
					if op.ExpectError {
						if err == nil {
							t.Fatalf("op %d: expected the append to be rejected, it succeeded", j)
						}
						continue
					}
					if err != nil {
						t.Fatalf("op %d: append: %v", j, err)
					}
					var want checkpoint.AppendResult
					if err := json.Unmarshal(op.Expect, &want); err != nil {
						t.Fatalf("op %d: bad expectation in fixture: %v", j, err)
					}
					if res != want {
						t.Fatalf("op %d: append result = %+v, want %+v\nwhy this case exists: %s", j, res, want, tc.Why)
					}

				case "load":
					state, err := cp.Load(ctx, runID)
					if err != nil {
						t.Fatalf("op %d: load: %v", j, err)
					}
					assertLoadMatches(t, j, state, op.Expect, tc.Why)

				case "query":
					state, err := cp.Load(ctx, runID)
					if err != nil {
						t.Fatalf("op %d: load for query: %v", j, err)
					}
					var want struct {
						Completed bool `json:"completed"`
						Attempted bool `json:"attempted"`
					}
					if err := json.Unmarshal(op.Expect, &want); err != nil {
						t.Fatalf("op %d: bad expectation in fixture: %v", j, err)
					}
					_, completed := state.Completed(op.StepID)
					if completed != want.Completed || state.Attempted(op.StepID) != want.Attempted {
						t.Fatalf("op %d: %s -> completed=%v attempted=%v, want completed=%v attempted=%v\nwhy this case exists: %s",
							j, op.StepID, completed, state.Attempted(op.StepID), want.Completed, want.Attempted, tc.Why)
					}

				default:
					t.Fatalf("op %d: unknown operation %q in the shared corpus", j, op.Op)
				}
			}
		})
	}
}

// assertLoadMatches compares next_seq and, when the case lists them, the projection of each record.
// recorded_at is deliberately not compared: it is a fact about the clock, not about the contract.
func assertLoadMatches(t *testing.T, opIndex int, state *checkpoint.RunState, expect json.RawMessage, why string) {
	t.Helper()
	var want struct {
		NextSeq *int64 `json:"next_seq"`
		Records *[]struct {
			StepID  string          `json:"step_id"`
			Phase   string          `json:"phase"`
			Seq     int64           `json:"seq"`
			Payload json.RawMessage `json:"payload"`
		} `json:"records"`
	}
	if err := json.Unmarshal(expect, &want); err != nil {
		t.Fatalf("op %d: bad load expectation in fixture: %v", opIndex, err)
	}

	if want.NextSeq != nil && state.NextSeq() != *want.NextSeq {
		t.Fatalf("op %d: next_seq = %d, want %d\nwhy this case exists: %s", opIndex, state.NextSeq(), *want.NextSeq, why)
	}
	if want.Records == nil {
		return
	}

	got := state.Records()
	if len(got) != len(*want.Records) {
		t.Fatalf("op %d: journal has %d records, want %d\nwhy this case exists: %s", opIndex, len(got), len(*want.Records), why)
	}
	for k, wantRec := range *want.Records {
		gotRec := got[k]
		if gotRec.StepID != wantRec.StepID || string(gotRec.Phase) != wantRec.Phase || gotRec.Seq != wantRec.Seq {
			t.Fatalf("op %d: record %d = (%s, %s, %d), want (%s, %s, %d)\nwhy this case exists: %s",
				opIndex, k, gotRec.StepID, gotRec.Phase, gotRec.Seq, wantRec.StepID, wantRec.Phase, wantRec.Seq, why)
		}
		if !jsonEqual(t, gotRec.Payload, wantRec.Payload) {
			t.Fatalf("op %d: record %d payload = %s, want %s\nwhy this case exists: %s", opIndex, k, gotRec.Payload, wantRec.Payload, why)
		}
	}
}

// jsonEqual compares two payloads semantically. Byte comparison would fail on key order, which is
// exactly the false alarm the contract says implementations must not raise.
func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	if len(a) == 0 || len(b) == 0 {
		return len(a) == 0 && len(b) == 0
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("stored payload is not valid JSON: %v", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("expected payload in fixture is not valid JSON: %v", err)
	}
	return reflect.DeepEqual(av, bv)
}
