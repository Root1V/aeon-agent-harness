package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// toolGatewayTracer emits OBS-001's "execute_tool" spans (OTel GenAI semantic conventions) around
// the policy-checked execution path. A no-op until some binary calls tracing.Init
// (go/internal/tracing) — safe regardless of whether tracing is wired up.
var toolGatewayTracer = otel.Tracer("aeon-toolgw")

// ToolGatewayHandlers exposes the policy-checked tool execution surface (TOOL-001's gateway half,
// SEC-001). Mirrors proto/aeon/v1/tool_gateway.proto's CheckPolicy/ExecuteTool RPCs over HTTP.
// The policy check always runs first and is never bypassable from this handler: /execute has no
// path that reaches the Executor without going through Policy.IsAllowed — see docs/adr/0001's
// "policy check after argument generation, before execution" rule.
type ToolGatewayHandlers struct {
	Policy   *policy.Engine
	Executor *toolexec.Executor
	// Executions is TOOL-005's dedupe table. Optional only in the sense that a deployment may not
	// configure it — a request that ASKS for deduplication and finds it missing is refused rather
	// than executed, because silently running an effect the caller asked to have deduplicated is
	// the failure this table exists to prevent.
	Executions *store.ToolExecutions
}

// Register mounts the tool gateway routes on mux.
func (h *ToolGatewayHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /check-policy", h.checkPolicy)
	mux.HandleFunc("POST /execute", h.execute)
}

type toolCallRequest struct {
	AgentManifestRef string         `json:"agent_manifest_ref"`
	ToolName         string         `json:"tool_name"`
	Args             map[string]any `json:"args"`
	// IdempotencyKey is derived by the caller from (run_id, node_id, step_seq, args) — see
	// python/aeon_worker/idempotency.py. Absent means "do not deduplicate", which is correct for a
	// read-only tool and wrong for anything with effects; the caller owns that choice because only
	// the caller knows which step it is on.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

func (h *ToolGatewayHandlers) checkPolicy(w http.ResponseWriter, r *http.Request) {
	var body toolCallRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	decision := h.Policy.IsAllowed(body.AgentManifestRef, body.ToolName)
	writeJSON(w, http.StatusOK, decision)
}

func (h *ToolGatewayHandlers) execute(w http.ResponseWriter, r *http.Request) {
	var body toolCallRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	_, span := toolGatewayTracer.Start(r.Context(), "execute_tool", trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", body.ToolName),
	))
	defer span.End()

	decision := h.Policy.IsAllowed(body.AgentManifestRef, body.ToolName)
	if !decision.Allowed {
		span.SetStatus(codes.Error, "denied by policy")
		writeJSON(w, http.StatusForbidden, map[string]any{
			"allowed": false,
			"reason":  "denied by policy",
		})
		return
	}

	if body.IdempotencyKey != "" {
		h.executeDeduplicated(w, r, span, body, decision.PolicyID)
		return
	}

	result, err := h.Executor.Execute(body.ToolName, body.Args)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		writeError(w, http.StatusNotFound, err)
		return
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"allowed":      true,
		"policy_id":    decision.PolicyID,
		"result":       result,
		"deduplicated": false,
	})
}

// executeDeduplicated is TOOL-005's path: claim the key, execute only if the claim was won, and
// record the outcome. The policy check has already run — deduplication never precedes it, or a
// replayed result could serve a call that policy would deny today.
func (h *ToolGatewayHandlers) executeDeduplicated(
	w http.ResponseWriter, r *http.Request, span trace.Span, body toolCallRequest, policyID string,
) {
	if h.Executions == nil {
		span.SetStatus(codes.Error, "dedupe requested but not configured")
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "idempotency_key was supplied but this gateway has no execution store configured — refusing to execute, because running an effect the caller asked to have deduplicated is exactly what the key was meant to prevent",
		})
		return
	}

	claim, err := h.Executions.Claim(r.Context(), body.IdempotencyKey, body.ToolName, body.AgentManifestRef, body.Args)
	if claim.ArgsDiverged {
		span.SetStatus(codes.Error, "idempotency key reused with different arguments")
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":           "this idempotency_key was already recorded against different arguments",
			"idempotency_key": body.IdempotencyKey,
		})
		return
	}
	if errors.Is(err, store.ErrExecutionInFlight) {
		span.SetStatus(codes.Error, "execution already in flight")
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":           err.Error(),
			"retryable":       true,
			"idempotency_key": body.IdempotencyKey,
		})
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if claim.Completed != nil {
		var recorded map[string]any
		if err := json.Unmarshal(claim.Completed, &recorded); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		span.SetStatus(codes.Ok, "deduplicated")
		writeJSON(w, http.StatusOK, map[string]any{
			"allowed":      true,
			"policy_id":    policyID,
			"result":       recorded,
			"deduplicated": true,
		})
		return
	}

	result, execErr := h.Executor.Execute(body.ToolName, body.Args)
	if execErr != nil {
		if relErr := h.Executions.Release(r.Context(), body.IdempotencyKey); relErr != nil {
			log.Printf("aeon-toolgw: releasing idempotency key after a failed execution: %v", relErr)
		}
		span.RecordError(execErr)
		span.SetStatus(codes.Error, execErr.Error())
		writeError(w, http.StatusNotFound, execErr)
		return
	}
	if err := h.Executions.Complete(r.Context(), body.IdempotencyKey, result); err != nil {
		// The effect already happened. Failing the request now would invite a retry that cannot be
		// deduplicated, so the honest answer is the result plus the fact that it is unprotected.
		log.Printf("aeon-toolgw: recording a completed execution: %v", err)
		span.SetStatus(codes.Error, "executed but not recorded")
		writeJSON(w, http.StatusOK, map[string]any{
			"allowed": true, "policy_id": policyID, "result": result,
			"deduplicated": false,
			"warning":      "executed, but the result could not be recorded: a retry with this key would execute again",
		})
		return
	}

	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"allowed":         true,
		"policy_id":       policyID,
		"result":          result,
		"deduplicated":    false,
		"failed_attempts": claim.FailedAttempts,
	})
}
