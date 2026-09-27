package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

// loadRepoBundle loads the CHECKED-IN policy bundle, so a test can assert on the file a deployment
// actually runs rather than on a copy of it.
//
// Both kinds of fixture are here on purpose and they answer different questions. testBundle below is a
// minimal one for Cedar semantics — it can be read in one screen, which is what a test of permit/forbid
// precedence needs. This one is for claims about OUR bundle, which only the real file can settle.
func loadRepoBundle(t *testing.T) *Engine {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "examples", "deep-research", "policy_bundle.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var doc PolicyBundleDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the bundle: %v", err)
	}
	e, err := LoadEngine(doc)
	if err != nil {
		t.Fatalf("LoadEngine: %v", err)
	}
	return e
}

// testBundle is a MINIMAL fixture for Cedar semantics: an agent may call its allowed tools, and shell.*
// is forbidden for everyone, which shows forbid winning over a permit that would otherwise match.
//
// It is NOT a mirror of examples/deep-research/policy_bundle.yaml, and the comment here used to say it
// was — which was already false by the time A2A-002 added `resource is Tool` to the real file. A fixture
// that claims to mirror a file is a duplicate that drifts silently; claims about the real bundle belong
// in a test that reads it (see TestDelegationIsNotGrantedByAToolPermit).
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

// TestDecisionNamesTheDeclaredPolicyID pins what a decision's PolicyID actually is.
//
// It used to be Cedar's positional name for a concatenated document — policy0, policy1, policy2 — which
// read like an identifier, did not match the `id:` printed in the bundle beside it, and changed whenever
// anyone reordered the file. INT-011 made it matter: once a denial is journalled as a durable fact,
// "denied by policy policy2" is a record that stops being true the next time the bundle is edited.
func TestDecisionNamesTheDeclaredPolicyID(t *testing.T) {
	doc := PolicyBundleDoc{Policies: []PolicyBundleItem{
		{ID: "allow-reads", Effect: "permit", CedarSource: `permit(principal == Agent::"a@1", action, resource) when { resource.name == "search.web" };`},
		{ID: "forbid-shell-for-everyone", Effect: "forbid", CedarSource: `forbid(principal, action, resource) when { resource.name like "shell.*" };`},
	}}
	engine, err := LoadEngine(doc)
	if err != nil {
		t.Fatalf("LoadEngine: %v", err)
	}

	denied := engine.IsAllowed("a@1", "shell.exec")
	if denied.Allowed {
		t.Fatal("shell.exec was allowed")
	}
	if denied.PolicyID != "forbid-shell-for-everyone" {
		t.Errorf("PolicyID = %q, want the id the bundle declares — a positional name sends an auditor to a policy that may no longer be there", denied.PolicyID)
	}

	allowed := engine.IsAllowed("a@1", "search.web")
	if !allowed.Allowed || allowed.PolicyID != "allow-reads" {
		t.Errorf("search.web = %+v, want allowed by allow-reads", allowed)
	}

	// Order must not change the names. This is the assertion the old behaviour could not have passed, and
	// the reason it is here rather than only in the denial test above.
	reordered := PolicyBundleDoc{Policies: []PolicyBundleItem{doc.Policies[1], doc.Policies[0]}}
	reorderedEngine, err := LoadEngine(reordered)
	if err != nil {
		t.Fatalf("LoadEngine reordered: %v", err)
	}
	if got := reorderedEngine.IsAllowed("a@1", "shell.exec").PolicyID; got != "forbid-shell-for-everyone" {
		t.Errorf("after reordering the bundle, PolicyID = %q — the identifier moved with the file", got)
	}
}

// TestDuplicatePolicyIDIsRefused guards the hole that keying by declared id opened.
//
// cedar-go's PolicySet.Add OVERWRITES a policy with the same id and reports it only in a return value,
// so a bundle with a repeated id would load, look fine, and enforce one of the two. The one that
// disappears could be a forbid, and a forbid that never loaded is a hole nothing downstream checks —
// the Tool Gateway would answer "allowed" with complete confidence.
func TestDuplicatePolicyIDIsRefused(t *testing.T) {
	_, err := LoadEngine(PolicyBundleDoc{Policies: []PolicyBundleItem{
		{ID: "same", CedarSource: `permit(principal == Agent::"a@1", action, resource) when { resource.name == "search.web" };`},
		{ID: "same", CedarSource: `forbid(principal, action, resource) when { resource.name like "shell.*" };`},
	}})
	if err == nil {
		t.Fatal("a bundle with a duplicated id loaded: one of the two policies is silently not enforced, and here it is the forbid")
	}
}

// TestPolicyWithoutIDIsRefused: an entry with no id could only be reported by position, which is the
// thing being removed. Failing at load is the only place this can be caught — by the time a decision is
// journalled, the record is already written.
func TestPolicyWithoutIDIsRefused(t *testing.T) {
	_, err := LoadEngine(PolicyBundleDoc{Policies: []PolicyBundleItem{
		{CedarSource: `forbid(principal, action, resource) when { resource.name like "shell.*" };`},
	}})
	if err == nil {
		t.Fatal("a bundle entry with no id loaded")
	}
}

// TestDelegationIsNotGrantedByAToolPermit is a regression test for a hole I MEASURED in the real bundle
// before fixing it (A2A-002).
//
// `allow-deep-research-tools` listed tool NAMES and never said what kind of thing it was naming, so a
// remote agent declared as "search.web" inherited the permit and delegation to it was authorized. The
// separate `RemoteAgent` entity type was necessary and not sufficient: without `resource is Tool` in the
// permit, the type was never consulted.
//
// This runs against the CHECKED-IN bundle, not an inline copy, because the thing that can regress is the
// bundle — someone tidying a `when` clause removes the guard and the leak comes back with every test
// still green.
func TestDelegationIsNotGrantedByAToolPermit(t *testing.T) {
	engine := loadRepoBundle(t)
	const agent = "deep-research-general@0.1.0"

	// The permits still do their job.
	if d := engine.IsAllowed(agent, "search.web"); !d.Allowed {
		t.Fatalf("search.web is no longer permitted (%+v) — the type guard broke tool authorization", d)
	}
	if d := engine.IsAllowedToDelegate(agent, "research-partner"); !d.Allowed {
		t.Fatalf("delegation to research-partner is not permitted (%+v)", d)
	}

	// The leak itself.
	if d := engine.IsAllowedToDelegate(agent, "search.web"); d.Allowed {
		t.Errorf("delegation to a REMOTE AGENT named %q was permitted by %q — a permit that lists names without saying what kind of thing they are lets a delegation destination borrow a tool's authorization", "search.web", d.PolicyID)
	}
	// And the mirror image, which would be just as wrong: a tool must not inherit a delegation permit.
	if d := engine.IsAllowed(agent, "research-partner"); d.Allowed {
		t.Errorf("a TOOL named %q was permitted by %q — the delegation permit is leaking the other way", "research-partner", d.PolicyID)
	}
}
