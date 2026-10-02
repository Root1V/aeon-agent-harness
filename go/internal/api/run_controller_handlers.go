package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/aeon-ai/aeon/go/internal/auth"
	"github.com/aeon-ai/aeon/go/internal/checkpoint"
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
	// Checkpointer journals a person's approval decision as a known outcome (INT-011).
	//
	// THE HARNESS WRITES THIS, not the loop, and that is the accepted contract consequence rather than a
	// shortcut: a person decides when the loop is not running, so the loop cannot record what it did not
	// witness. Registering a fact is not deciding anything, which is what keeps the seam's promise intact.
	//
	// Optional. Without it a decision still takes effect — the response says it was not journalled.
	Checkpointer checkpoint.Checkpointer
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

func workflowID(runID string) string { return runcontroller.WorkflowIDPrefix + runID }

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

// approver resolves who is deciding a pending approval, or writes the refusal and returns false.
//
// TWO CHECKS AND THEY ARE DIFFERENT QUESTIONS. The first is whether anybody is authenticated at all;
// the second is whether that somebody may approve. `mayApprove` is never implied by `mayActAs`
// (go/internal/auth), and this is the endpoint that distinction exists for: a worker holding a
// credential good enough to run tools must not be able to approve the irreversible call it is itself
// blocked on, or the gate is decoration with a journal entry.
func (h *RunControllerHandlers) approver(w http.ResponseWriter, r *http.Request) (auth.Caller, bool) {
	caller, ok := auth.CallerFrom(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "unauthenticated: this run controller was reached without an authenticated caller (SEC-005)",
		})
		return auth.Caller{}, false
	}
	if !caller.MayApprove {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":     fmt.Sprintf("caller %q may not decide approvals", caller.ID),
			"caller_id": caller.ID,
		})
		return auth.Caller{}, false
	}
	return caller, true
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
	caller, ok := h.approver(w, r)
	if !ok {
		return
	}
	decision, err := h.Controller.Approve(r.Context(), workflowID(r.PathValue("run_id")), body.ToolCallHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	h.respondToApproval(w, r, caller, decision)
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
	caller, ok := h.approver(w, r)
	if !ok {
		return
	}
	decision, err := h.Controller.Reject(r.Context(), workflowID(r.PathValue("run_id")), body.ToolCallHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	h.respondToApproval(w, r, caller, decision)
}

// respondToApproval journals the decision and answers 202 with what was recorded.
//
// Ordering is the opposite of the Tool Gateway's denial path, and deliberately so. The gateway journals
// BEFORE responding because the refusal is already final when Cedar returns; here the decision only
// becomes true when Temporal accepts the signal, so journalling first would assert something that had
// not happened yet. The rule both follow: write the record as soon as the fact is true, and never sooner.
func (h *RunControllerHandlers) respondToApproval(
	w http.ResponseWriter, r *http.Request, caller auth.Caller, decision runcontroller.ApprovalDecision,
) {
	journal := h.journalApproval(r.Context(), r.PathValue("run_id"), caller, decision)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"approval_id":    decision.ApprovalID,
		"tool_call_hash": decision.ToolCallHash,
		"outcome":        checkpoint.OutcomeForApproval(decision.Approved),
		// Echoed back, because the caller that approved should be able to see the name the journal
		// recorded rather than trust that it matched.
		"decided_by": caller.ID,
		"journalled": journal.Journalled,
		"journal":    journal,
	})
}

func (h *RunControllerHandlers) journalApproval(
	ctx context.Context, runID string, caller auth.Caller, decision runcontroller.ApprovalDecision,
) approvalJournalResult {
	if h.Checkpointer == nil {
		return approvalJournalResult{Reason: "this control plane has no checkpointer configured"}
	}
	if decision.ApprovalID == "" {
		// The run answered the query without an approval_id, so there is no stable identity to key the
		// record on. Recording it under a made-up id would be worse than not recording it: the entry would
		// never collapse with a retry of the same decision.
		return approvalJournalResult{Reason: "the pending approval carried no approval_id to key the record on"}
	}

	// node_id and tool_call_hash travel in the payload rather than the key: they are what a resuming loop
	// matches against to know WHICH call the person allowed, and the hash is what makes a mutated argument
	// fail closed (see go/internal/stepidentity).
	detail, err := json.Marshal(map[string]any{
		"node_id":        decision.NodeID,
		"tool_call_hash": decision.ToolCallHash,
		"approved":       decision.Approved,
	})
	if err != nil {
		return approvalJournalResult{Reason: err.Error()}
	}
	// SEC-005: the record names WHO, and the reason stops claiming what this code could not know.
	//
	// It used to read "decided by a person via the run controller". Nothing verified that: the endpoint
	// was unauthenticated, so the only true statement was "something that could reach the port". An
	// audit line asserting a person was involved, on a step whose whole purpose is that a person was
	// involved, is the most expensive kind of nearly-correct — and the kind this repo keeps finding in
	// its own artefacts. Now the kind comes from the caller's own declaration in the bundle.
	payload, err := checkpoint.OutcomePayloadDecidedBy(
		checkpoint.OutcomeForApproval(decision.Approved),
		fmt.Sprintf("decided by %s %q via the run controller", caller.Kind, caller.ID),
		caller.ID,
		detail,
	)
	if err != nil {
		return approvalJournalResult{Reason: err.Error()}
	}

	stepID := checkpoint.ApprovalStep(decision.ApprovalID)
	res, err := h.Checkpointer.Append(ctx, checkpoint.Entry{
		RunID: runID, StepID: stepID, Phase: checkpoint.PhaseCompleted, Payload: payload,
	})
	if err != nil {
		log.Printf("aeon-controlplane: journalling approval %s for run %s: %v", decision.ApprovalID, runID, err)
		return approvalJournalResult{Reason: err.Error()}
	}
	return approvalJournalResult{
		Journalled: true, StepID: stepID, Seq: res.Seq, Duplicate: res.Duplicate,
	}
}

// approvalJournalResult reports whether the decision was recorded, and why not when it was not.
type approvalJournalResult struct {
	Journalled bool   `json:"journalled"`
	StepID     string `json:"step_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Seq        int64  `json:"seq,omitempty"`
	Duplicate  bool   `json:"duplicate,omitempty"`
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
