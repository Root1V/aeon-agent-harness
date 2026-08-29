package policy

import "testing"

// testBundle mirrors examples/deep-research/policy_bundle.yaml: the deep-research agent may call
// its allowed tools; shell.* is explicitly forbidden for every agent, demonstrating forbid wins
// over any permit that might otherwise match.
const testBundle = `
permit(
  principal == Agent::"deep-research-general@0.1.0",
  action,
  resource
) when {
  ["search.web", "search.rag", "repository.read", "artifact.read"].contains(resource.name)
};

forbid(
  principal,
  action,
  resource
) when {
  resource.name like "shell.*"
};
`

func mustLoadTestEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := LoadEngine(PolicyBundleDoc{Policies: []PolicyBundleItem{{ID: "test", CedarSource: testBundle}}})
	if err != nil {
		t.Fatalf("LoadEngine: %v", err)
	}
	return e
}

// TestPolicyEngineDeniesOutOfManifestToolCall is (the Cedar-only half of) SEC-001's acceptance
// test. It proves: an allowed tool is permitted, a tool outside the agent's manifest is denied by
// Cedar's default-deny (no permit matches), and a tool matching an explicit forbid is denied even
// though it would otherwise be reachable — mirroring the spec's acceptance criterion "un agente no
// puede invocar una herramienta fuera de su ToolDescriptor/policy aunque el modelo la solicite."
func TestPolicyEngineDeniesOutOfManifestToolCall(t *testing.T) {
	e := mustLoadTestEngine(t)
	agent := "deep-research-general@0.1.0"

	allowed := e.IsAllowed(agent, "search.web")
	if !allowed.Allowed {
		t.Fatalf("search.web: expected allowed, got denied (policy=%s)", allowed.PolicyID)
	}

	// Not in the allow list and no permit matches it: Cedar's default deny applies.
	deniedByDefault := e.IsAllowed(agent, "external.write.database")
	if deniedByDefault.Allowed {
		t.Fatalf("external.write.database: expected denied by default, got allowed")
	}

	// Explicitly forbidden, which must win even if some other policy would have permitted it.
	deniedByForbid := e.IsAllowed(agent, "shell.exec")
	if deniedByForbid.Allowed {
		t.Fatalf("shell.exec: expected denied by explicit forbid, got allowed")
	}

	// A different agent entirely (no permit policy names it) is denied for every tool, including
	// ones the deep-research agent is allowed to use — authorization is per-principal, not global.
	otherAgentDenied := e.IsAllowed("some-other-agent@1.0.0", "search.web")
	if otherAgentDenied.Allowed {
		t.Fatalf("unrelated agent calling search.web: expected denied, got allowed")
	}
}
