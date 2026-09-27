// Package policy is the Cedar-based Policy Engine (SEC-001, docs/adr/0002). Authorization is
// evaluated here, entirely outside the model: the Tool Gateway calls Engine.IsAllowed after the
// model has generated a tool call's arguments and before that call is ever executed. Cedar's
// default is deny — a tool call is only allowed if an explicit `permit` policy matches it, and any
// matching `forbid` policy always wins over a `permit` (see examples/deep-research/policy_bundle.yaml).
package policy

import (
	"fmt"

	"github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/types"
)

// PolicyBundleDoc mirrors proto/manifests/policy_bundle.schema.json.
type PolicyBundleDoc struct {
	APIVersion   string             `json:"apiVersion" yaml:"apiVersion"`
	Kind         string             `json:"kind" yaml:"kind"`
	CedarVersion string             `json:"cedarVersion" yaml:"cedarVersion"`
	Policies     []PolicyBundleItem `json:"policies" yaml:"policies"`
}

// PolicyBundleItem is one policy within a PolicyBundleDoc.
type PolicyBundleItem struct {
	ID          string `json:"id" yaml:"id"`
	Effect      string `json:"effect" yaml:"effect"`
	CedarSource string `json:"cedarSource" yaml:"cedarSource"`
	// Disposition is INT-010: what the loop should DO when this policy determines the decision.
	//
	// It lives in the BUNDLE because Cedar is binary — it answers allow or deny and nothing richer — while
	// the seam has to distinguish "refuse this step" from "end the run" and "allowed" from "allowed once a
	// person says yes". Those are governance statements, so they belong in the versioned config the
	// governance team writes, not in code that would be guessing on their behalf.
	//
	// Optional. An omitted disposition is DERIVED from the effect, which is faithful rather than assumed —
	// see derivedDisposition.
	Disposition Disposition `json:"disposition,omitempty" yaml:"disposition,omitempty"`
}

// Engine evaluates tool-call authorization against a loaded Cedar policy set.
type Engine struct {
	policySet *cedar.PolicySet
	// dispositions maps a policy id to what the bundle declared for it (INT-010). Keyed by the id Cedar
	// reports in its diagnostic, which is the SAME id the bundle declares only because LoadEngine now
	// keys the policy set by it — before that it was a positional index, and a lookup here would have
	// silently missed every time anyone reordered the file.
	dispositions map[cedar.PolicyID]Disposition
}

// LoadEngine parses every policy's cedarSource into a single Cedar PolicySet, KEYED BY THE ID THE
// BUNDLE DECLARES.
//
// Policy by policy rather than one concatenated document, and the difference is not stylistic. Cedar
// names the policies in a concatenated document by POSITION — policy0, policy1, policy2 — so the id
// reported in a decision used to be an index into the file. It looked like an identifier: an audit
// record said "denied by policy policy2" while the bundle right next to it said
// `id: forbid-shell-for-everyone`, two namespaces that resemble each other closely enough to be
// mistaken. And it moved: inserting a policy at the top of the bundle silently renamed every id below
// it, so yesterday's audit trail described today's policies wrongly.
//
// Found by INT-011, which journals a denial as a durable fact — the moment the id had to survive being
// read back later, a positional index stopped being good enough.
func LoadEngine(doc PolicyBundleDoc) (*Engine, error) {
	ps := cedar.NewPolicySet()
	dispositions := map[cedar.PolicyID]Disposition{}
	for i, p := range doc.Policies {
		if p.ID == "" {
			return nil, fmt.Errorf("policy: bundle entry %d declares no id: a decision it determines could only be reported by position, which changes whenever the bundle is reordered", i)
		}
		if err := validateDeclaredDisposition(p.Effect, p.Disposition); err != nil {
			return nil, fmt.Errorf("policy: bundle entry %q: %w", p.ID, err)
		}
		list, err := cedar.NewPolicyListFromBytes(p.ID, []byte(p.CedarSource))
		if err != nil {
			return nil, fmt.Errorf("policy: parse cedar policy %q: %w", p.ID, err)
		}
		if len(list) == 0 {
			return nil, fmt.Errorf("policy: bundle entry %q has no policy in its cedarSource: an entry that authorizes nothing but reads as though it does is worse than a missing one", p.ID)
		}
		for n, parsed := range list {
			// One id per entry in the ordinary case. An entry holding several policies gets a suffix rather
			// than an error: the bundle schema does not forbid it, and losing one to a name collision would
			// be a policy silently not enforced.
			id := cedar.PolicyID(p.ID)
			if len(list) > 1 {
				id = cedar.PolicyID(fmt.Sprintf("%s#%d", p.ID, n))
			}
			// Add OVERWRITES a policy with the same id and only reports it in its return value, so a bundle
			// with a duplicated id would quietly enforce one of the two. Refused here: the one that vanishes
			// could be a forbid, and a forbid that is not loaded is a hole nothing else in the system checks.
			if !ps.Add(id, parsed) {
				return nil, fmt.Errorf("policy: bundle declares id %q more than once: one of them would silently replace the other", id)
			}
			if p.Disposition != "" {
				dispositions[id] = p.Disposition
			}
		}
	}
	return &Engine{policySet: ps, dispositions: dispositions}, nil
}

// Decision is the result of an authorization check.
//
// Allowed means "may execute NOW", which is narrower than "was not forbidden" and deliberately so: a
// require_approval decision reports Allowed FALSE, so every call site that reads only this field fails
// closed instead of running an effect nobody approved. See Disposition.PermitsExecution.
type Decision struct {
	Allowed       bool
	PolicyID      string
	CedarDecision string
	// Disposition is INT-010: what the loop should do. Never empty — a decision with no declared
	// disposition carries the one derived from the effect.
	Disposition Disposition `json:"disposition"`
	// DispositionDeclared says whether the bundle stated it or we derived it from the effect. Reported
	// rather than folded in, because "nobody has thought about this policy's disposition" and "somebody
	// decided deny_step" are different facts about a bundle, and only the first is worth an operator's
	// attention.
	DispositionDeclared bool `json:"disposition_declared"`
}

// IsAllowed evaluates whether agentManifestRef (e.g. "deep-research-general@0.1.0") may invoke
// toolName. This is the entire authorization surface SEC-001 promises: the model never sees or
// influences this decision, and it runs after argument generation, before execution (ADR-001/ADR-002).
func (e *Engine) IsAllowed(agentManifestRef, toolName string) Decision {
	return e.isAllowedFor("Agent", agentManifestRef, toolName)
}

// IsAllowedForPrincipal evaluates authorization for a caller that isn't an Aeon Agent — e.g. an
// external MCP client (INT-003) reaching Aeon's governed tool catalog directly, with no
// AgentManifest of its own. principalType names the Cedar entity type (e.g. "McpClient");
// principalID is the specific identity within that type. A policy bundle authorizes this exactly
// like an Agent: an explicit `permit` naming this principal, evaluated by the same Cedar engine.
func (e *Engine) IsAllowedForPrincipal(principalType, principalID, toolName string) Decision {
	return e.isAllowedFor(principalType, principalID, toolName)
}

// IsAllowedToDelegate evaluates whether agentManifestRef may delegate to a declared remote agent
// (A2A-002). The resource is a `RemoteAgent`, NOT a `Tool`, and that separation is the point.
//
// Reusing `Tool` would have made a bundle that permits a list of tool names accidentally cover
// delegation the moment someone named a remote agent like a tool — and, worse, a permit with no `when`
// clause would grant delegation to anyone it granted tools to. A distinct entity type makes "may call
// these tools" and "may hand work to this third party" two statements a bundle has to make separately,
// which is what they are: the second sends an effect outside our perimeter, where the child's tools
// cross ITS gateway and not ours.
func (e *Engine) IsAllowedToDelegate(agentManifestRef, remoteAgentID string) Decision {
	return e.authorize("Agent", agentManifestRef, "RemoteAgent", remoteAgentID)
}

func (e *Engine) isAllowedFor(principalType, principalID, toolName string) Decision {
	return e.authorize(principalType, principalID, "Tool", toolName)
}

func (e *Engine) authorize(principalType, principalID, resourceType, resourceName string) Decision {
	principal := types.NewEntityUID(types.EntityType(principalType), types.String(principalID))
	action := types.NewEntityUID(types.EntityType("Action"), types.String(resourceName))
	resourceUID := types.NewEntityUID(types.EntityType(resourceType), types.String(resourceName))

	entities := types.EntityMap{
		resourceUID: types.Entity{
			UID: resourceUID,
			Attributes: types.NewRecord(types.RecordMap{
				types.String("name"): types.String(resourceName),
			}),
		},
	}

	req := cedar.Request{
		Principal: principal,
		Action:    action,
		Resource:  resourceUID,
		Context:   types.Record{},
	}

	decision, diagnostic := cedar.Authorize(e.policySet, entities, req)

	policyID := ""
	if len(diagnostic.Reasons) > 0 {
		policyID = string(diagnostic.Reasons[0].PolicyID)
	}

	allowed := decision == types.Allow
	// The STRICTEST declaration among every determining policy wins, and all of them are scanned rather
	// than just Reasons[0]. Cedar can report several: if two permits match and only one says a person must
	// approve, honouring the other would run the call and ask nobody — the comfortable error again.
	disposition, declared := derivedDisposition(allowed), false
	for _, reason := range diagnostic.Reasons {
		if d, ok := e.dispositions[reason.PolicyID]; ok {
			disposition, declared = stricter(disposition, d), true
		}
	}

	return Decision{
		Allowed:       allowed && disposition.PermitsExecution(),
		PolicyID:      policyID,
		CedarDecision: decision.String(),

		Disposition:         disposition,
		DispositionDeclared: declared,
	}
}
