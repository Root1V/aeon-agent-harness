package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/aeon-ai/aeon/go/internal/runcontroller"
)

// RunControllerHandlers exposes RUN-001: start/cancel/pause/resume/status/stream for a run, as a
// thin HTTP layer over runcontroller.Controller (which does the actual Temporal client calls).
type RunControllerHandlers struct {
	Controller *runcontroller.Controller
}

// Register mounts the run controller routes on mux.
func (h *RunControllerHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /runs", h.start)
	mux.HandleFunc("GET /runs/{run_id}", h.status)
	mux.HandleFunc("POST /runs/{run_id}/cancel", h.cancel)
	mux.HandleFunc("POST /runs/{run_id}/pause", h.pause)
	mux.HandleFunc("POST /runs/{run_id}/resume", h.resume)
	mux.HandleFunc("POST /runs/{run_id}/approve", h.approve)
	mux.HandleFunc("POST /runs/{run_id}/reject", h.reject)
	mux.HandleFunc("GET /runs/{run_id}/stream", h.stream)
}

func workflowID(runID string) string { return "graph-run-" + runID }

type startRunRequest struct {
	RunID   string         `json:"run_id"`
	Graph   map[string]any `json:"graph"`
	Budgets map[string]any `json:"budgets,omitempty"` // RUN-003: optional {max_tool_calls, max_depth, deadline_seconds}
}

func (h *RunControllerHandlers) start(w http.ResponseWriter, r *http.Request) {
	var body startRunRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.RunID == "" || body.Graph == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("run_id and graph are required"))
		return
	}
	info, err := h.Controller.Start(r.Context(), body.RunID, body.Graph, body.Budgets)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (h *RunControllerHandlers) status(w http.ResponseWriter, r *http.Request) {
	status, err := h.Controller.Status(r.Context(), workflowID(r.PathValue("run_id")))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *RunControllerHandlers) cancel(w http.ResponseWriter, r *http.Request) {
	if err := h.Controller.Cancel(r.Context(), workflowID(r.PathValue("run_id"))); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *RunControllerHandlers) pause(w http.ResponseWriter, r *http.Request) {
	if err := h.Controller.Pause(r.Context(), workflowID(r.PathValue("run_id"))); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *RunControllerHandlers) resume(w http.ResponseWriter, r *http.Request) {
	if err := h.Controller.Resume(r.Context(), workflowID(r.PathValue("run_id"))); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

type approvalDecisionRequest struct {
	ToolCallHash string `json:"tool_call_hash"`
}

func (h *RunControllerHandlers) approve(w http.ResponseWriter, r *http.Request) {
	var body approvalDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.ToolCallHash == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("tool_call_hash is required"))
		return
	}
	if err := h.Controller.Approve(r.Context(), workflowID(r.PathValue("run_id")), body.ToolCallHash); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *RunControllerHandlers) reject(w http.ResponseWriter, r *http.Request) {
	var body approvalDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.ToolCallHash == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("tool_call_hash is required"))
		return
	}
	if err := h.Controller.Reject(r.Context(), workflowID(r.PathValue("run_id")), body.ToolCallHash); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// stream is a Server-Sent Events endpoint: polls Status and emits an event each time it changes,
// until a terminal status (SUCCEEDED/FAILED/CANCELLED) is reached. Simple polling rather than
// subscribing to Temporal's own event stream — sufficient for RUN-001's acceptance bar and keeps
// this handler decoupled from Temporal's history API.
func (h *RunControllerHandlers) stream(w http.ResponseWriter, r *http.Request) {
	wfID := workflowID(r.PathValue("run_id"))
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	var last string
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	for {
		status, err := h.Controller.Status(r.Context(), wfID)
		if err != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
			flusher.Flush()
			return
		}
		if status.Status != last {
			payload, _ := json.Marshal(status)
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
			last = status.Status
		}
		if isTerminal(status.Status) {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func isTerminal(status string) bool {
	switch status {
	case "SUCCEEDED", "FAILED", "CANCELLED":
		return true
	default:
		return false
	}
}
