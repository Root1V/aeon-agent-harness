package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/checkpoint"
)

// The HTTP half of VRT-AEON-004: the contract Veritium's own HttpCheckpointer consumes.
//
// Their ask had two parts and this covers the second one — "que el cliente no tenga que elegir". A
// caller with no delegation sends no `sub_run_id` at all and the row still gets a definite value,
// because the sentinel is a schema default rather than something a client has to invent.
func TestTheCheckpointRouteCarriesTheSubRun(t *testing.T) {
	s := newAPITestStore(t)
	mux := http.NewServeMux()
	(&CheckpointHandlers{Store: s}).Register(mux)
	srv := httptest.NewServer(authWrap(t, mux))
	t.Cleanup(srv.Close)

	runID := "route-subrun-" + randSuffix(t)
	post := func(t *testing.T, subRunID, stepID string) map[string]any {
		t.Helper()
		body, _ := json.Marshal(appendCheckpointRequest{
			SubRunID: subRunID, StepID: stepID, Phase: string(checkpoint.PhaseCompleted),
			Payload: json.RawMessage(`{"ok":true}`),
		})
		resp := postJSONAuthed(t, srv.URL+"/runs/"+runID+"/checkpoints", body)
		defer resp.Body.Close()
		var parsed map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&parsed)
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("POST for sub-run %q: status=%d body=%v", subRunID, resp.StatusCode, parsed)
		}
		return parsed
	}

	// The field OMITTED entirely — not sent as "" — because that is what a client that knows nothing
	// about delegation does, and it is the case the schema default exists for.
	rootBody, _ := json.Marshal(map[string]any{
		"step_id": "s1", "phase": string(checkpoint.PhaseCompleted), "payload": map[string]any{"ok": true},
	})
	resp := postJSONAuthed(t, srv.URL+"/runs/"+runID+"/checkpoints", rootBody)
	func() {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("POST with no sub_run_id at all: status=%d", resp.StatusCode)
		}
	}()

	// The same step id under two delegations. Before VRT-AEON-004 the second answered
	// duplicate=true and wrote nothing, which is a lost record reported as a success.
	for _, path := range []string{"child-a", "child-b/grandchild"} {
		if dup, _ := post(t, path, "s1")["duplicate"].(bool); dup {
			t.Fatalf("sub-run %q was journalled as a duplicate of another sub-run's step", path)
		}
	}

	state := loadRunState(t, srv, runID)
	if got := len(state.Records()); got != 3 {
		t.Fatalf("the journal holds %d records over the wire, want 3", got)
	}
	// Read back through the SAME sub-run the writer used, which is the only reading that proves the
	// field survived the round trip rather than being dropped by the request struct or the response.
	for _, path := range []string{"", "child-a", "child-b/grandchild"} {
		if _, ok := state.Completed(path, "s1"); !ok {
			t.Errorf("no record for sub-run %q after a round trip through the route — the field is "+
				"dropped on the way in or on the way out", path)
		}
	}
	// And a genuine retry of one of them is still a duplicate over HTTP.
	if dup, _ := post(t, "child-a", "s1")["duplicate"].(bool); !dup {
		t.Error("an identical retry for one sub-run was not reported as a duplicate")
	}
}
