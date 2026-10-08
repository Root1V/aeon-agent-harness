package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/aeon-ai/aeon/go/internal/auth"
	"github.com/aeon-ai/aeon/go/internal/checkpoint"
	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
	"github.com/aeon-ai/aeon/go/internal/tracing"
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
	// Policy is the SET — one Cedar engine per tenant (VRT-AEON-005 T-5). Resolved per request from
	// the caller's tenant, and a tenant with no bundle is DENIED rather than falling back to
	// somebody else's permits.
	Policy   *policy.Set
	Executor *toolexec.Executor
	// Executions is the STORE, not a dedupe handle, since VRT-AEON-005: the handle is derived per
	// request from the caller's tenant (store.ToolExecutionsFor), because a handle built once at
	// startup would be one tenant's for every caller. Optional only in the sense that a deployment
	// may not configure it — a request that ASKS for deduplication and finds it missing is refused
	// rather than executed, because silently running an effect the caller asked to have
	// deduplicated is the failure this table exists to prevent.
	Executions *store.Store
	// Checkpointer journals a policy denial as a KNOWN OUTCOME of the step (INT-011).
	//
	// The gateway writes it rather than the caller, and that placement is the feature. A caller that
	// journalled its own 403 would lose the fact whenever it died between receiving the refusal and
	// recording it — which is exactly the crash window the journal exists to survive. The gateway is
	// the only party that knows the denial happened at the moment it happens.
	//
	// Optional: a call that carries no run_id/step_id (a Mode C MCP client with no Aeon run behind it)
	// has nothing to journal AGAINST, and a deployment may have no journal at all. Neither is allowed to
	// turn a denial into an error — but neither is allowed to look like a recorded denial either, which
	// is why the response says which of the three happened.
	// VRT-AEON-005: the store; the journal handle is derived per request from the caller's tenant.
	Checkpointer *store.Store
}

// Register mounts the tool gateway routes on mux.

// engineFor resolves the Cedar engine for the caller's tenant, writing the refusal itself when there
// is none.
//
// THREE REFUSALS, AND THEY SAY DIFFERENT THINGS ON PURPOSE. No caller means this route was mounted
// without auth.Require — a wiring mistake, and answering anyway is how an unauthenticated surface
// comes back. No bundle for the tenant means governance has not been written for them yet, which is
// a deployment gap and not a policy decision; it must not resolve to another tenant's engine,
// because a permit applying where nobody wrote it is the exact defect A2A-002 and RUN-006 both hit.
func (h *ToolGatewayHandlers) engineFor(w http.ResponseWriter, r *http.Request) (*policy.Engine, string, bool) {
	caller, ok := auth.CallerFrom(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "unauthenticated: policy is evaluated against the caller's tenant (SEC-005/VRT-AEON-005)",
		})
		return nil, "", false
	}
	engine, ok := h.Policy.EngineFor(caller.Tenant)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "no policy bundle is loaded for this caller's tenant, so nothing is authorized for it. " +
				"A missing bundle is a denial and never a fallback to another tenant's policies",
			"tenant": caller.Tenant,
		})
		return nil, "", false
	}
	return engine, caller.Tenant, true
}

func (h *ToolGatewayHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /check-policy", h.checkPolicy)
	mux.HandleFunc("POST /check-activity-policy", h.checkActivityPolicy)
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
	// RunID and StepID identify the journal entry a denial is recorded against (INT-011). Optional,
	// because the gateway serves callers with no Aeon run behind them; absent means the denial cannot be
	// journalled, which the response reports rather than hides.
	RunID  string `json:"run_id,omitempty"`
	StepID string `json:"step_id,omitempty"`
}

func (h *ToolGatewayHandlers) checkPolicy(w http.ResponseWriter, r *http.Request) {
	var body toolCallRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	engine, _, ok := h.engineFor(w, r)
	if !ok {
		return
	}
	decision := engine.IsAllowed(body.AgentManifestRef, body.ToolName)
	writeJSON(w, http.StatusOK, decision)
}

// activityPolicyRequest is VRT-AEON-001's question: may this agent have the run schedule this named
// activity on this task queue?
type activityPolicyRequest struct {
	AgentManifestRef string `json:"agent_manifest_ref"`
	ActivityName     string `json:"activity_name"`
	TaskQueue        string `json:"task_queue"`
}

// checkActivityPolicy authorizes an external activity before the workflow schedules it.
//
// A SEPARATE ENDPOINT AND NOT A FIELD ON /check-policy. A flag would have made one request shape
// mean two kinds of resource, and the failure mode is the one A2A-002 measured: a caller that omits
// the discriminator gets the other kind's answer. Separate routes cannot be confused by omission.
//
// AND IT ENFORCES SEC-005, which /check-policy above does not — read that as the asymmetry it is.
// /check-policy is advisory: the enforcement point for a tool is /execute, where this gateway both
// decides AND executes, so a wrong answer to an advisory question changes nothing. For an external
// activity there is no such endpoint and there cannot be: the worker that serves the activity
// belongs to the consumer, so Aeon is not in the data path. The only thing Aeon controls is whether
// its own workflow schedules the work — which makes THIS answer the decision, and a decision taken
// on a principal the caller merely asserted is exactly what SEC-005 removed from /execute.
//
// The boundary that remains, stated plainly rather than oversold: this governs what an Aeon run will
// schedule. It does not stop a consumer's own code from putting a task on its own queue by itself.
// What it buys is that a governed run cannot be the thing that does it.
func (h *ToolGatewayHandlers) checkActivityPolicy(w http.ResponseWriter, r *http.Request) {
	var body activityPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.ActivityName == "" || body.TaskQueue == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "activity_name and task_queue are both required: the queue decides whose worker " +
				"picks the work up, so a decision without it would authorize a name on any queue",
		})
		return
	}

	caller, ok := auth.CallerFrom(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "unauthenticated: this gateway was reached without an authenticated caller (SEC-005)",
		})
		return
	}
	if !caller.ActsAs(body.AgentManifestRef) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":              "caller may not act as this agent (SEC-005)",
			"caller_id":          caller.ID,
			"agent_manifest_ref": body.AgentManifestRef,
		})
		return
	}

	engine, _, ok := h.engineFor(w, r)
	if !ok {
		return
	}
	decision := engine.IsAllowedToRunActivity(body.AgentManifestRef, body.ActivityName, body.TaskQueue)
	writeJSON(w, http.StatusOK, decision)
}

func (h *ToolGatewayHandlers) execute(w http.ResponseWriter, r *http.Request) {
	var body toolCallRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	ctx, span := toolGatewayTracer.Start(r.Context(), "execute_tool", trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", body.ToolName),
	))
	defer span.End()

	// SEC-005: the caller may only present an agent its own entry lists.
	//
	// THIS IS THE LINE THAT MAKES CEDAR MEAN SOMETHING. Until it existed, the principal came straight
	// off the wire — `IsAllowed(body.AgentManifestRef, ...)` — so the engine was default-deny, well
	// tested, and judging whichever identity the caller typed. Every `permit` in the bundle was
	// reachable by anyone who could reach the port and knew the agent's name, which is in the bundle.
	//
	// REFUSED WHEN THERE IS NO CALLER AT ALL, not treated as a legacy path. An absent caller means this
	// handler was mounted without auth.Require, and answering it anyway is how an unauthenticated route
	// comes back one wiring mistake at a time.
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		span.SetStatus(codes.Error, "unauthenticated")
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "unauthenticated: this gateway was reached without an authenticated caller (SEC-005)",
		})
		return
	}
	span.SetAttributes(attribute.String("aeon.caller.id", caller.ID), attribute.String("aeon.caller.kind", string(caller.Kind)))
	if !caller.ActsAs(body.AgentManifestRef) {
		// 403 and not 404: the caller is known and the refusal is about what it may claim. And the
		// message names both sides, because "forbidden" without them sends an operator to the Cedar
		// bundle, which is the wrong file — the policy never got a say.
		span.SetStatus(codes.Error, "caller may not act as this agent")
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":              fmt.Sprintf("caller %q may not act as %q", caller.ID, body.AgentManifestRef),
			"caller_id":          caller.ID,
			"agent_manifest_ref": body.AgentManifestRef,
		})
		return
	}

	engine, _, ok := h.engineFor(w, r)
	if !ok {
		return
	}
	decision := engine.IsAllowed(body.AgentManifestRef, body.ToolName)
	if !decision.Allowed {
		// NOT codes.Error, and this corrects a contradiction that sat in this file for a while: the
		// MarkGuardrail comment said a policy denial is the system working correctly and must not be
		// reported as an error, while this line marked it as one three functions away.
		//
		// It matters beyond tidiness. Argus's router pages on `status.code == STATUS_CODE_ERROR` as well as
		// on argus.guardrail, so removing the guardrail from a denial changed nothing until this changed
		// too — measured: the hot-path counter still moved by 1. A refusal is this gateway doing its job,
		// so the span describes a successful operation whose OUTCOME was `denied`.
		span.SetStatus(codes.Ok, "denied by policy")
		span.SetAttributes(
			attribute.String("aeon.policy.disposition", string(decision.Disposition)),
			attribute.Bool("aeon.policy.disposition_declared", decision.DispositionDeclared),
		)
		// Argus's hot path (2s) rather than the 30-60s cold one. The KIND comes from the disposition, because
		// "a person must approve this" and "policy forbids this" need answering differently and within
		// seconds of each other — see go/internal/tracing/argus.go.
		tracing.MarkOutcome(span, tracing.OutcomeForDisposition(decision.Disposition))
		span.SetAttributes(attribute.String("aeon.guardrail.reason", "denied by policy "+decision.PolicyID))

		// Journalled BEFORE the response, on purpose: after it, a crash in this process between writing
		// the 403 and writing the record would leave the step looking unfinished, which is the exact
		// state INT-011 exists to eliminate. The order costs one durable write on a path that is not
		// executing anything anyway.
		//
		// INT-010 changes WHICH outcome, not the ordering: a require_approval step is not denied, it is
		// waiting, so nothing is journalled here and the approval's own outcome is written later by the
		// harness when a person decides. Recording a denial now would log a refusal nobody made, and the
		// run would read as closed while it is suspended.
		outcome, isDenial := dispositionOutcome(decision.Disposition)
		journal := denialResult{Reason: "not a denial: the step is waiting for a person, and its outcome is journalled when the approval is decided"}
		if isDenial {
			journal = h.journalDenial(r.Context(), body, outcome, denialReason(decision))
			span.SetAttributes(attribute.String("aeon.step.outcome", string(outcome)))
		}
		span.SetAttributes(attribute.Bool("aeon.step.outcome_journalled", journal.Journalled))

		writeJSON(w, http.StatusForbidden, map[string]any{
			"allowed": false,
			"reason":  "denied by policy",
			// The outcome is named in the response too, so a caller that DOES keep its own journal records
			// the same fact under the same name instead of inventing one. Absent for require_approval,
			// because there is no outcome yet — a person has not decided.
			"outcome":    outcomeOrEmpty(outcome),
			"journalled": journal.Journalled,
			"journal":    journal,

			// INT-010: WHAT TO DO, not merely that it was refused. A boolean tells the loop the call failed
			// and leaves it to choose between trying something else, ending the run, and asking a person —
			// three different behaviours that `allowed: false` cannot distinguish.
			"disposition":          decision.Disposition,
			"disposition_declared": decision.DispositionDeclared,
			"policy_id":            decision.PolicyID,
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

	// The tenant comes from the credential, never from the request (T-1). A dedupe table keyed
	// without it meant the same idempotency key in two tenants was one row, so the second tenant's
	// call came back "already executed" carrying the first tenant's recorded result.
	caller, ok := auth.CallerFrom(r.Context())
	if !ok {
		span.SetStatus(codes.Error, "unauthenticated")
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "unauthenticated: deduplication is scoped to the caller's tenant (SEC-005/VRT-AEON-005)",
		})
		return
	}
	executions := h.Executions.ToolExecutionsFor(caller.Tenant)

	claim, err := executions.Claim(r.Context(), body.IdempotencyKey, body.ToolName, body.AgentManifestRef, body.Args)
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
		if relErr := executions.Release(r.Context(), body.IdempotencyKey); relErr != nil {
			log.Printf("aeon-toolgw: releasing idempotency key after a failed execution: %v", relErr)
		}
		span.RecordError(execErr)
		span.SetStatus(codes.Error, execErr.Error())
		writeError(w, http.StatusNotFound, execErr)
		return
	}
	if err := executions.Complete(r.Context(), body.IdempotencyKey, result); err != nil {
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

// denialResult reports what happened to the journal write for a denial.
//
// Three states rather than a bool, the same rule this codebase applies to every counter: "recorded",
// "there was nothing to record it against", and "we tried and failed" are different facts, and only
// the last one is a problem. Collapsing them would make an unjournalled denial in a Mode C call look
// identical to a Postgres outage.
type denialResult struct {
	Journalled bool `json:"journalled"`
	// Reason is empty when Journalled is true — a fact needs no excuse.
	Reason string `json:"reason,omitempty"`
	Seq    int64  `json:"seq,omitempty"`
	// Duplicate is true when this denial was already journalled: a retried call that policy denies again
	// is the same outcome, not a second one.
	Duplicate bool `json:"duplicate,omitempty"`
}

// journalDenial records a refused step as a completed one with an explicit denied outcome.
//
// It NEVER fails the request. A denial whose record could not be written is still a denial, and
// turning it into a 500 would mean a journal outage could get an effect executed on retry — the
// opposite of what a policy denial is for. The caller learns the record is missing instead.
func (h *ToolGatewayHandlers) journalDenial(
	ctx context.Context, body toolCallRequest, outcome checkpoint.Outcome, reason string,
) denialResult {
	if body.RunID == "" || body.StepID == "" {
		return denialResult{Reason: "no run_id/step_id on the request: there is no journal to record this against"}
	}
	if h.Checkpointer == nil {
		return denialResult{Reason: "this gateway has no checkpointer configured"}
	}

	payload, err := checkpoint.OutcomePayload(outcome, reason, nil)
	if err != nil {
		return denialResult{Reason: err.Error()}
	}
	res, err := h.Checkpointer.CheckpointerFor(callerTenantOrEmpty(ctx)).Append(ctx, checkpoint.Entry{
		RunID: body.RunID, StepID: body.StepID, Phase: checkpoint.PhaseCompleted, Payload: payload,
	})
	if err != nil {
		// Logged as well as returned: the caller sees it, and so does whoever is reading the gateway's
		// logs when a run turns out to have a hole in its journal.
		log.Printf("aeon-toolgw: journalling a policy denial for run %s step %s: %v", body.RunID, body.StepID, err)
		return denialResult{Reason: err.Error()}
	}
	return denialResult{Journalled: true, Seq: res.Seq, Duplicate: res.Duplicate}
}

// denialReason is the human-readable half of the journalled record.
//
// It names the policy that refused when Cedar identified one, because "denied by policy" alone sends
// whoever is debugging back to reading the whole bundle. Cedar is default-deny, so no policy id is
// itself informative: it means nothing permitted the call rather than something forbade it.
func denialReason(decision policy.Decision) string {
	if decision.PolicyID != "" {
		return "denied by policy " + decision.PolicyID
	}
	return "denied by policy: no policy in the bundle permits this tool for this agent (Cedar is default-deny)"
}

// dispositionOutcome maps a policy disposition to the journal outcome it produces (INT-010 x INT-011).
//
// require_approval maps to NOTHING, and that is the case worth stating: the step is not denied, it is
// waiting. Its outcome is written later by the harness, when a person actually decides — see
// RunControllerHandlers.journalApproval.
func dispositionOutcome(d policy.Disposition) (checkpoint.Outcome, bool) {
	switch d {
	case policy.DispositionDenyStep, policy.DispositionTerminateRun:
		return checkpoint.OutcomeDeniedByPolicy, true
	default:
		return "", false
	}
}

// outcomeOrEmpty keeps an absent outcome out of the JSON as "" rather than as a made-up value.
func outcomeOrEmpty(o checkpoint.Outcome) any {
	if o == "" {
		return nil
	}
	return o
}

// A POLICY REFUSAL NO LONGER SETS argus.guardrail AT ALL, on Argus's own recommendation (2026-09-29), and
// that replaces what this function used to do.
//
// Their router triggers on the PRESENCE of argus.guardrail, so setting it is requesting a page within two
// seconds — whatever the value. Two consequences they pointed out and we had wrong:
//
//   - A require_approval step is normal operation. Paging on every approval would be alert fatigue built
//     from inside, so it gets no guardrail at all.
//   - A Cedar denial may be ROUTINE in a given deployment, and a guardrail that fires on routine is noise
//     with a good name. So a denial carries `argus.outcome=denied` instead, which their gateway's audit
//     policy retains in full — measured by them: with only outcome=denied, sampling kept 0 of 8 denials
//     until `denied` entered the audit policy, then 8 of 8.
//
// What remains a guardrail here is nothing: the tool gateway's refusals are all policy decisions. Fan-out
// and undeclared destinations still are guardrails, in the A2A egress path, because a runaway and a
// misconfiguration are both rare and both worth a page.
