package finops

import "fmt"

// Ceiling is what an AgentManifest declares a run may spend: `spec.runtime.budgets`.
//
// WHY THE MANIFEST AND NOT THE REQUEST. A caller that declares its own ceiling can raise it, which
// makes the ceiling a suggestion. The manifest is the artefact an auditor reads and a release gate
// checks, and `examples/deep-research/agent.yaml` has carried `budgets: {modelCalls: 60, costUsd: 5.0}`
// since it was written — with nothing enforcing either. `costUsd` had zero references in the Go tree.
//
// POINTERS, BECAUSE ABSENT IS NOT ZERO. A manifest that declares no cost ceiling must mean "no
// ceiling", and a 0.0 would mean "spend nothing" — i.e. refuse the first call of every run. Collapsing
// those two is how a budget feature takes a deployment down the day it ships.
type Ceiling struct {
	CostUSD    *float64
	ModelCalls *int
}

// Declared reports whether this manifest asks for any enforcement at all.
func (c Ceiling) Declared() bool { return c.CostUSD != nil || c.ModelCalls != nil }

func (c Ceiling) String() string {
	switch {
	case c.CostUSD != nil && c.ModelCalls != nil:
		return fmt.Sprintf("costUsd=%.4f modelCalls=%d", *c.CostUSD, *c.ModelCalls)
	case c.CostUSD != nil:
		return fmt.Sprintf("costUsd=%.4f", *c.CostUSD)
	case c.ModelCalls != nil:
		return fmt.Sprintf("modelCalls=%d", *c.ModelCalls)
	}
	return "none declared"
}

// CeilingFromManifest reads spec.runtime.budgets out of a stored AgentManifest.
//
// Every value that is present but not a number is IGNORED rather than treated as zero, for the reason
// above: a typo in a budget field must not become a ceiling of nothing. It is reported through the
// second return value so a caller can log it — a manifest whose budget does not parse is a manifest
// whose author believes it is enforced.
func CeilingFromManifest(manifest map[string]any) (Ceiling, []string) {
	var c Ceiling
	var problems []string

	spec, _ := manifest["spec"].(map[string]any)
	runtime, _ := spec["runtime"].(map[string]any)
	budgets, _ := runtime["budgets"].(map[string]any)
	if budgets == nil {
		return c, nil
	}

	if raw, present := budgets["costUsd"]; present {
		if f, ok := asFloat(raw); ok && f >= 0 {
			c.CostUSD = &f
		} else {
			problems = append(problems, fmt.Sprintf("costUsd is %v (%T), which is not a non-negative number — NOT enforced", raw, raw))
		}
	}
	if raw, present := budgets["modelCalls"]; present {
		if f, ok := asFloat(raw); ok && f >= 0 {
			n := int(f)
			c.ModelCalls = &n
		} else {
			problems = append(problems, fmt.Sprintf("modelCalls is %v (%T), which is not a non-negative number — NOT enforced", raw, raw))
		}
	}
	return c, problems
}

// asFloat accepts what JSON and YAML decoding actually produce for a number.
func asFloat(raw any) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}
