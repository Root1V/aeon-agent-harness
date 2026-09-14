package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/aeon-ai/aeon/go/internal/runcontroller"
	"github.com/aeon-ai/aeon/go/internal/store"
)

// runControllerTracer emits OBS-001's "invoke_agent" span (OTel GenAI semantic conventions) around
// starting a run — the root of a run's trace. A no-op until some binary calls tracing.Init
// (go/internal/tracing) — safe regardless of whether tracing is wired up.
var runControllerTracer = otel.Tracer("aeon-runcontroller")

// RunControllerHandlers exposes RUN-001: start/cancel/pause/resume/status/stream for a run, as a
// thin HTTP layer over runcontroller.Controller (which does the actual Temporal client calls).
// Registry is optional (A5): when set and a start request names agent_manifest_ref, a quarantined
// version's run is refused before Controller.Start is ever called — the circuit breaker's
// enforcement point. Nil, or an omitted agent_manifest_ref, skips the check entirely (unaffected,
// pre-A5 behavior).
type RunControllerHandlers struct {
	Controller *runcontroller.Controller
	Registry   *store.AgentRegistry
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
	// AgentManifestRef (A5, optional): "name@version" of the AgentManifest this run belongs to. When
	// set and Registry is configured, a quarantined version is refused here — a real circuit
	// breaker enforcement point, not just an advisory flag. Omitted entirely: unaffected.
	AgentManifestRef string `json:"agent_manifest_ref,omitempty"`
}

// checkNotQuarantined enforces A5's circuit breaker at the one place that matters: before a new run
// is ever started. A missing registry, an empty ref, a malformed ref, or an unknown agent are all
// treated as "nothing to enforce" — this check's only job is to refuse a KNOWN, quarantined agent
// version, never to validate ref shape or agent existence (that's other code's job).
func checkNotQuarantined(ctx context.Context, registry *store.AgentRegistry, agentManifestRef string) error {
	if registry == nil || agentManifestRef == "" {
		return nil
	}
	name, version, ok := strings.Cut(agentManifestRef, "@")
	if !ok {
		return nil
	}
	rec, err := registry.Get(ctx, name, version)
	if err != nil {
		return nil
	}
	if rec.Quarantined {
		return fmt.Errorf("agent %s is quarantined: %s", agentManifestRef, rec.QuarantineReason)
	}
	return nil
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

	ctx, span := runControllerTracer.Start(r.Context(), "invoke_agent", trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "invoke_agent"),
		attribute.String("gen_ai.agent.name", body.RunID),
	))
	defer span.End()

	if err := checkNotQuarantined(ctx, h.Registry, body.AgentManifestRef); err != nil {
		span.SetStatus(codes.Error, "quarantined")
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error()})
		return
	}

	info, err := h.Controller.Start(ctx, body.RunID, body.Graph, body.Budgets, body.AgentManifestRef)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	span.SetStatus(codes.Ok, "")
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
