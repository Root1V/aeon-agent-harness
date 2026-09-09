package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aeon-ai/aeon/go/internal/checkpoint"
	"github.com/aeon-ai/aeon/go/internal/store"
)

func newCheckpointTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	s, err := store.Connect(context.Background(), memoryTestDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)

	mux := http.NewServeMux()
	(&CheckpointHandlers{Checkpointer: s.Checkpointer()}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postCheckpoint(t *testing.T, srv *httptest.Server, runID string, body map[string]any) (int, checkpoint.AppendResult) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/runs/"+runID+"/checkpoints", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST checkpoint: %v", err)
	}
	defer resp.Body.Close()
	var parsed checkpoint.AppendResult
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}

// TestCheckpointSeamOverHTTP proves the seam is reachable from outside this process — the framework
// that uses it is a Python one, so the Go interface alone would not be a boundary.
func TestCheckpointSeamOverHTTP(t *testing.T) {
	srv := newCheckpointTestServer(t)
	runID := fmt.Sprintf("cp-http-%d", time.Now().UnixNano())

	t.Run("a new entry is 201 and a repeat is 200 with duplicate=true", func(t *testing.T) {
		body := map[string]any{"step_id": "s1", "phase": "completed", "payload": map[string]any{"ok": true}}

		status, first := postCheckpoint(t, srv, runID, body)
		if status != http.StatusCreated || first.Duplicate {
			t.Fatalf("first append = %d %+v, want 201 and not a duplicate", status, first)
		}

		status, again := postCheckpoint(t, srv, runID, body)
		if status != http.StatusOK || !again.Duplicate {
			t.Fatalf("repeat append = %d %+v, want 200 and duplicate=true", status, again)
		}
		if again.Seq != first.Seq {
			t.Errorf("repeat seq = %d, want %d", again.Seq, first.Seq)
		}
	})

	t.Run("an unknown phase is rejected at the boundary", func(t *testing.T) {
		// phase is half the idempotency key, so a typo must fail loudly here rather than quietly
		// journal a second entry for a step that already ran.
		status, _ := postCheckpoint(t, srv, runID, map[string]any{"step_id": "s2", "phase": "finished"})
		if status != http.StatusBadRequest {
			t.Fatalf("append with phase=finished = %d, want 400", status)
		}
	})

	t.Run("loading returns the journal and where it continues", func(t *testing.T) {
		postCheckpoint(t, srv, runID, map[string]any{"step_id": "s3", "phase": "attempted"})

		resp, err := http.Get(srv.URL + "/runs/" + runID + "/checkpoints")
		if err != nil {
			t.Fatalf("GET checkpoints: %v", err)
		}
		defer resp.Body.Close()
		var state runStateResponse
		if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
			t.Fatalf("decoding run state: %v", err)
		}
		if state.RunID != runID || state.NextSeq != 2 || len(state.Records) != 2 {
			t.Fatalf("state = %+v, want run %s, next_seq 2 and 2 records", state, runID)
		}
		if state.Records[0].Seq != 0 || state.Records[1].Seq != 1 {
			t.Errorf("records are not in sequence order: %+v", state.Records)
		}
	})

	t.Run("an unknown run loads as an empty journal, not an error", func(t *testing.T) {
		// A loop asking "where was I?" before its first checkpoint is the normal first call, not a
		// failure — answering 404 would make every caller special-case the happy path.
		resp, err := http.Get(srv.URL + "/runs/never-seen-" + runID + "/checkpoints")
		if err != nil {
			t.Fatalf("GET checkpoints: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET for an unknown run = %d, want 200", resp.StatusCode)
		}
		var state runStateResponse
		_ = json.NewDecoder(resp.Body).Decode(&state)
		if state.NextSeq != 0 || len(state.Records) != 0 {
			t.Errorf("state for an unknown run = %+v, want an empty journal starting at 0", state)
		}
	})
}
