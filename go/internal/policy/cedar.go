// Package policy is the Cedar-based Policy Engine (SEC-001, docs/adr/0002). Authorization is
// evaluated here, entirely outside the model: the Tool Gateway calls Engine.IsAllowed after the
// model has generated a tool call's arguments and before that call is ever executed. Cedar's
// default is deny — a tool call is only allowed if an explicit `permit` policy matches it, and any
// matching `forbid` policy always wins over a `permit` (see examples/deep-research/policy_bundle.yaml).
package policy

import (
	"bytes"
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
}

// Engine evaluates tool-call authorization against a loaded Cedar policy set.
type Engine struct {
	policySet *cedar.PolicySet
}

// LoadEngine parses every policy's cedarSource into a single Cedar PolicySet.
func LoadEngine(doc PolicyBundleDoc) (*Engine, error) {
	var buf bytes.Buffer
	for _, p := range doc.Policies {
		buf.WriteString(p.CedarSource)
		buf.WriteString("\n")
	}
	ps, err := cedar.NewPolicySetFromBytes("policy_bundle", buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("policy: parse cedar bundle: %w", err)
	}
	return &Engine{policySet: ps}, nil
}

// Decision is the result of an authorization check: whether the call is allowed, and — for
// audit/debugging — which policy (if any) determined the outcome.
type Decision struct {
	Allowed       bool
	PolicyID      string
	CedarDecision string
}

// IsAllowed evaluates whether agentManifestRef (e.g. "deep-research-general@0.1.0") may invoke
// toolName. This is the entire authorization surface SEC-001 promises: the model never sees or
// influences this decision, and it runs after argument generation, before execution (ADR-001/ADR-002).
func (e *Engine) IsAllowed(agentManifestRef, toolName string) Decision {
	principal := types.NewEntityUID(types.EntityType("Agent"), types.String(agentManifestRef))
	action := types.NewEntityUID(types.EntityType("Action"), types.String(toolName))
	resourceUID := types.NewEntityUID(types.EntityType("Tool"), types.String(toolName))

	entities := types.EntityMap{
		resourceUID: types.Entity{
			UID: resourceUID,
			Attributes: types.NewRecord(types.RecordMap{
				types.String("name"): types.String(toolName),
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

	return Decision{
		Allowed:       decision == types.Allow,
		PolicyID:      policyID,
		CedarDecision: decision.String(),
	}
}
