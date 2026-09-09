package api

import (
	"encoding/json"
	"net/http"

	"github.com/aeon-ai/aeon/go/internal/checkpoint"
)

// CheckpointHandlers exposes INT-009's durability seam over HTTP. This is what makes it a seam at
// all: the framework on the other side of it (Synaptum) is a Python process, so a Go interface
// nothing outside this binary can call would be a library, not a boundary.
//
// The surface is exactly the two operations of checkpoint.Checkpointer and no third. Convenience
// endpoints answering "should I re-run this step?" were left out on purpose — that question is the
// loop's to answer, and putting it here would move a decision to the side of the seam that agreed
// not to make any.
type CheckpointHandlers struct {
	Checkpointer checkpoint.Checkpointer
}

// Register mounts the checkpoint routes on mux.
func (h *CheckpointHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /runs/{run_id}/checkpoints", h.append)
	mux.HandleFunc("GET /runs/{run_id}/checkpoints", h.load)
}

type appendCheckpointRequest struct {
	StepID  string          `json:"step_id"`
	Phase   string          `json:"phase"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// append journals one entry. A duplicate answers 200 with duplicate=true; a genuinely new entry
// answers 201. Both are successes — the seam's contract is that a caller under at-least-once
// execution must never have to distinguish a retry from a first attempt in order to stay correct.
func (h *CheckpointHandlers) append(w http.ResponseWriter, r *http.Request) {
	var body appendCheckpointRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	entry := checkpoint.Entry{
		RunID:   r.PathValue("run_id"),
		StepID:  body.StepID,
		Phase:   checkpoint.Phase(body.Phase),
		Payload: body.Payload,
	}
	if err := entry.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	res, err := h.Checkpointer.Append(r.Context(), entry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	status := http.StatusCreated
	if res.Duplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, res)
}

type runStateResponse struct {
	RunID   string              `json:"run_id"`
	NextSeq int64               `json:"next_seq"`
	Records []checkpoint.Record `json:"records"`
}

func (h *CheckpointHandlers) load(w http.ResponseWriter, r *http.Request) {
	state, err := h.Checkpointer.Load(r.Context(), r.PathValue("run_id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	records := state.Records()
	if records == nil {
		records = []checkpoint.Record{}
	}
	writeJSON(w, http.StatusOK, runStateResponse{RunID: state.RunID(), NextSeq: state.NextSeq(), Records: records})
}
