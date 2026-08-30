package api

import (
	"encoding/json"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/aeon-ai/aeon/go/internal/policy"
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

	result, err := h.Executor.Execute(body.ToolName, body.Args)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		writeError(w, http.StatusNotFound, err)
		return
	}
	span.SetStatus(codes.Ok, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"allowed":   true,
		"policy_id": decision.PolicyID,
		"result":    result,
	})
}
