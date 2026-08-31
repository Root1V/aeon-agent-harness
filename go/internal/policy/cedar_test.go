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

// TestPolicyEngineIsAllowedForPrincipalSupportsNonAgentPrincipals is INT-003's underlying
// requirement: an external MCP client isn't an Agent::"name@version" — it needs its own Cedar
// entity type, and a policy bundle must be able to permit it independently of any Agent policy.
func TestPolicyEngineIsAllowedForPrincipalSupportsNonAgentPrincipals(t *testing.T) {
	bundle := testBundle + `
permit(
  principal == McpClient::"external-mcp-client",
  action,
  resource
) when {
  ["search.web"].contains(resource.name)
};
`
	e, err := LoadEngine(PolicyBundleDoc{Policies: []PolicyBundleItem{{ID: "test", CedarSource: bundle}}})
	if err != nil {
		t.Fatalf("LoadEngine: %v", err)
	}

	allowed := e.IsAllowedForPrincipal("McpClient", "external-mcp-client", "search.web")
	if !allowed.Allowed {
		t.Fatalf("search.web for the permitted McpClient: expected allowed, got denied (policy=%s)", allowed.PolicyID)
	}

	// The universal forbid still applies to a non-Agent principal — forbid is principal-agnostic.
	forbidden := e.IsAllowedForPrincipal("McpClient", "external-mcp-client", "shell.exec")
	if forbidden.Allowed {
		t.Fatal("shell.exec for McpClient: expected denied by explicit forbid, got allowed")
	}

	// A different McpClient identity, not named by any permit, is denied by default — same
	// per-principal isolation IsAllowed already gives Agent callers.
	otherClientDenied := e.IsAllowedForPrincipal("McpClient", "some-other-client", "search.web")
	if otherClientDenied.Allowed {
		t.Fatal("unrelated McpClient calling search.web: expected denied, got allowed")
	}

	// IsAllowed (Agent-scoped) and IsAllowedForPrincipal("Agent", ...) must agree — they're the
	// same evaluation, just reached through the convenience wrapper vs. the general one.
	viaWrapper := e.IsAllowed("deep-research-general@0.1.0", "search.web")
	viaGeneral := e.IsAllowedForPrincipal("Agent", "deep-research-general@0.1.0", "search.web")
	if viaWrapper.Allowed != viaGeneral.Allowed {
		t.Errorf("IsAllowed/IsAllowedForPrincipal(\"Agent\",...) disagree: %v vs %v", viaWrapper.Allowed, viaGeneral.Allowed)
	}
}
