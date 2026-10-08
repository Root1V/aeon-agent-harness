package policy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// TestDispositionComesFromTheBundleNotFromCode is INT-010's policy-level half.
//
// Cedar answers allow or deny and nothing richer, so the four dispositions have to come from somewhere.
// They come from the BUNDLE — they are governance statements, and the alternative was code guessing on the
// governance team's behalf.
func TestDispositionComesFromTheBundleNotFromCode(t *testing.T) {
	t.Run("an omitted disposition is DERIVED from the effect, and says it was", func(t *testing.T) {
		// deny_step is not a guess: it is exactly what a Cedar `forbid` says, and nothing more. What makes
		// the default acceptable is that terminate_run asserts something Cedar does NOT say, so a bundle has
		// to opt into it — and that the decision reports which of the two happened.
		e, err := LoadEngine(PolicyBundleDoc{Policies: []PolicyBundleItem{
			{ID: "forbid-plain", Effect: "forbid", CedarSource: `forbid(principal, action, resource) when { resource.name == "x" };`},
		}})
		if err != nil {
			t.Fatalf("LoadEngine: %v", err)
		}
		d := e.IsAllowed("a@1", "x")
		if d.Disposition != DispositionDenyStep {
			t.Errorf("disposition = %q, want %q", d.Disposition, DispositionDenyStep)
		}
		if d.DispositionDeclared {
			t.Error("reported as declared when nothing declared it — an operator cannot then tell a considered deny_step from one nobody thought about")
		}
	})

	t.Run("require_approval permits nothing YET, so allowed is false", func(t *testing.T) {
		// The most important assertion in this file. Every existing call site reads `if !decision.Allowed
		// { refuse }`. If a require_approval decision left Allowed true, all of them would execute the
		// effect without anybody being asked — so the seam has to fail closed by construction, and only
		// code that understands dispositions can act on the difference.
		e, err := LoadEngine(PolicyBundleDoc{Policies: []PolicyBundleItem{
			{ID: "ask-first", Effect: "permit", Disposition: DispositionRequireApproval,
				CedarSource: `permit(principal == Agent::"a@1", action, resource) when { resource.name == "danger" };`},
		}})
		if err != nil {
			t.Fatalf("LoadEngine: %v", err)
		}
		d := e.IsAllowed("a@1", "danger")
		if d.Allowed {
			t.Fatal("allowed is true for a require_approval policy — every call site that reads only this field would run the effect with nobody asked")
		}
		if d.Disposition != DispositionRequireApproval {
			t.Errorf("disposition = %q, want %q", d.Disposition, DispositionRequireApproval)
		}
		if d.CedarDecision != "allow" {
			t.Errorf("CedarDecision = %q, want allow — Cedar DID permit it; the seam is what withholds execution, and conflating the two would hide which of them refused", d.CedarDecision)
		}
	})

	t.Run("the STRICTEST declaration among matching policies wins", func(t *testing.T) {
		// Cedar can report several determining policies. If two permits match and only one says a person
		// must approve, honouring the other would run the call and ask nobody.
		e, err := LoadEngine(PolicyBundleDoc{Policies: []PolicyBundleItem{
			{ID: "broad-allow", Effect: "permit",
				CedarSource: `permit(principal == Agent::"a@1", action, resource);`},
			{ID: "narrow-ask", Effect: "permit", Disposition: DispositionRequireApproval,
				CedarSource: `permit(principal == Agent::"a@1", action, resource) when { resource.name == "danger" };`},
		}})
		if err != nil {
			t.Fatalf("LoadEngine: %v", err)
		}
		if d := e.IsAllowed("a@1", "danger"); d.Allowed || d.Disposition != DispositionRequireApproval {
			t.Errorf("decision = %+v, want require_approval and allowed=false — a blanket permit must not cancel a narrower policy that asks for a person", d)
		}
		// And a tool only the broad permit covers is still plainly allowed, or the strictness rule would be
		// quietly gating everything.
		if d := e.IsAllowed("a@1", "ordinary"); !d.Allowed || d.Disposition != DispositionAllow {
			t.Errorf("decision = %+v, want a plain allow", d)
		}
	})

	t.Run("terminate_run outranks deny_step when both forbids match", func(t *testing.T) {
		e, err := LoadEngine(PolicyBundleDoc{Policies: []PolicyBundleItem{
			{ID: "soft", Effect: "forbid", Disposition: DispositionDenyStep,
				CedarSource: `forbid(principal, action, resource) when { resource.name like "shell.*" };`},
			{ID: "hard", Effect: "forbid", Disposition: DispositionTerminateRun,
				CedarSource: `forbid(principal, action, resource) when { resource.name == "shell.exec" };`},
		}})
		if err != nil {
			t.Fatalf("LoadEngine: %v", err)
		}
		if d := e.IsAllowed("a@1", "shell.exec"); d.Disposition != DispositionTerminateRun {
			t.Errorf("disposition = %q, want terminate_run", d.Disposition)
		}
		if d := e.IsAllowed("a@1", "shell.sh"); d.Disposition != DispositionDenyStep {
			t.Errorf("disposition = %q, want deny_step — only the narrower policy terminates", d.Disposition)
		}
	})

	t.Run("a contradiction between effect and disposition fails at LOAD", func(t *testing.T) {
		// It has to fail here: by evaluation time the bundle is already in force, and whichever half of the
		// contradiction the code happened to honour would silently become the policy.
		for _, tc := range []struct {
			name        string
			effect      string
			disposition Disposition
		}{
			{"a forbid that allows", "forbid", DispositionAllow},
			{"a forbid that asks for approval", "forbid", DispositionRequireApproval},
			{"a permit that denies the step", "permit", DispositionDenyStep},
			{"a permit that terminates the run", "permit", DispositionTerminateRun},
			{"an invented disposition", "forbid", Disposition("burn_it_down")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := LoadEngine(PolicyBundleDoc{Policies: []PolicyBundleItem{
					{ID: "contradictory", Effect: tc.effect, Disposition: tc.disposition,
						CedarSource: `forbid(principal, action, resource) when { resource.name == "x" };`},
				}})
				if err == nil {
					t.Fatalf("%s loaded", tc.name)
				}
			})
		}
	})

	t.Run("the checked-in bundle declares the four cases it means to", func(t *testing.T) {
		// Against the real file, because the dispositions ARE the bundle: a test over an inline copy would
		// verify that the mechanism works while saying nothing about what our deployment actually enforces.
		e := loadRepoBundle(t)
		const agent = "deep-research-general@0.1.0"
		for _, tc := range []struct {
			tool        string
			allowed     bool
			disposition Disposition
			declared    bool
		}{
			{"search.web", true, DispositionAllow, false},
			// shell.* ends the run: an agent trying for a shell is not making a recoverable mistake, and
			// handing back the refusal would invite shell.sh, then bash, then sh.
			{"shell.exec", false, DispositionTerminateRun, true},
			// A write outside the perimeter IS recoverable — the refusal going back as evidence is what lets
			// the model reach for the governed tool instead.
			{"external.write.database", false, DispositionDenyStep, true},
			// Permitted, but only once a person says yes.
			{"artifact.write", false, DispositionRequireApproval, true},
			// Default-deny: nothing permitted it, nothing declared anything.
			{"nothing.permits.this", false, DispositionDenyStep, false},
		} {
			t.Run(tc.tool, func(t *testing.T) {
				d := e.IsAllowed(agent, tc.tool)
				if d.Allowed != tc.allowed || d.Disposition != tc.disposition || d.DispositionDeclared != tc.declared {
					t.Errorf("decision = %+v, want allowed=%v disposition=%q declared=%v",
						d, tc.allowed, tc.disposition, tc.declared)
				}
			})
		}
	})
}

// TestAnExternalActivityDoesNotInheritAToolPermit is VRT-AEON-001's acceptance criterion 6, and I
// asked for it in the channel because it is the one that would regress silently.
//
// A2A-002 measured the hole once already: `allow-deep-research-tools` lists NAMES, so a resource that
// borrowed a tool's name inherited its permit. An external activity is the third kind of thing to pass
// through this engine, and it is the one with the widest blast radius — a permitted `activity_name`
// means a run can hand work to a worker process Aeon does not own, on a task queue it does not serve.
//
// Runs against the CHECKED-IN bundle, because what can regress is the bundle: someone tidying a `when`
// clause removes `resource is Tool` and the leak comes back with every other test still green.
func TestAnExternalActivityDoesNotInheritAToolPermit(t *testing.T) {
	engine := loadRepoBundle(t)
	const agent = "deep-research-general@0.1.0"

	t.Run("an activity named exactly like a permitted tool is DENIED", func(t *testing.T) {
		d := engine.IsAllowedToRunActivity(agent, "search.web", "some-queue")
		if d.Allowed {
			t.Errorf("an EXTERNAL ACTIVITY named %q was permitted by %q — scheduling work on somebody "+
				"else's worker must be its own statement in the bundle, not a side effect of permitting "+
				"a tool with the same name", "search.web", d.PolicyID)
		}
	})

	t.Run("the tool permit still works, so the guard did not break authorization", func(t *testing.T) {
		if d := engine.IsAllowed(agent, "search.web"); !d.Allowed {
			t.Fatalf("search.web is no longer permitted (%+v) — negative control: if this fails, the "+
				"test above proves nothing", d)
		}
	})

	t.Run("a FORBID with no type guard still catches an activity, and that asymmetry is correct", func(t *testing.T) {
		// `forbid-shell-for-everyone` matches on `resource.name like "shell.*"` with NO `resource is`
		// guard. For a permit that would be the hole above; for a forbid it is the right breadth — a
		// forbid that only covered tools would let `shell.exec` through as an activity name, which is
		// the same effect by a different door. Fixed here so a future tidy-up does not "make the
		// policies consistent" by adding a guard to the forbids.
		d := engine.IsAllowedToRunActivity(agent, "shell.exec", "some-queue")
		if d.Allowed {
			t.Fatal("an activity named shell.exec was permitted")
		}
		if d.Disposition != DispositionTerminateRun {
			t.Errorf("disposition = %q, want %q: the bundle declares terminate_run for shell, and an "+
				"agent trying to get a shell through the activity door is the same decision",
				d.Disposition, DispositionTerminateRun)
		}
	})

	t.Run("and the mirror image: a TOOL does not inherit the activity permit", func(t *testing.T) {
		// The bundle's `allow-test-external-activity` is guarded with `resource is ExternalActivity`.
		// Without that guard it would also authorize a tool of the same name, which is the A2A-002
		// hole pointing the other way — so both directions are fixed, not just the one that bit.
		if d := engine.IsAllowed("activity-node-test@0.1.0", "aeon.test.external_activity"); d.Allowed {
			t.Errorf("a TOOL named %q was permitted by %q — the activity permit is leaking the other way",
				"aeon.test.external_activity", d.PolicyID)
		}
	})

	t.Run("the task queue is a resource attribute a bundle can constrain", func(t *testing.T) {
		e, err := LoadEngine(PolicyBundleDoc{Policies: []PolicyBundleItem{{
			ID:     "allow-acme-online-only",
			Effect: "permit",
			CedarSource: `permit(principal == Agent::"p@1", action, resource) when {
				resource is ExternalActivity &&
				resource.name == "acme.process_item" &&
				resource.task_queue == "acme-online"
			};`,
		}}})
		if err != nil {
			t.Fatalf("LoadEngine: %v", err)
		}
		if d := e.IsAllowedToRunActivity("p@1", "acme.process_item", "acme-online"); !d.Allowed {
			t.Fatalf("the permitted queue was refused: %+v", d)
		}
		if d := e.IsAllowedToRunActivity("p@1", "acme.process_item", "acme-masivo"); d.Allowed {
			t.Error("the same activity on a DIFFERENT queue was permitted — the queue decides whose " +
				"worker picks the work up, so a bundle has to be able to say which one")
		}
	})
}

// TestABundleWithNoPoliciesIsRefused is the one-level-up version of the guard right beside it, which
// refuses an entry whose cedarSource holds no policy.
//
// MEASURED RATHER THAN IMAGINED, on 2026-10-08, while writing VRT-AEON-005's propagation test: a
// bundle whose policies were written under `statements:` instead of `policies:` loaded CLEANLY with
// zero policies, because an unrecognised key is silently dropped by the YAML decoder. The engine then
// denied everything — fail-closed, so not a hole — and the only symptom was "denied by policy" on a
// request the file plainly permits. The time went into debugging Cedar rather than the key.
func TestABundleWithNoPoliciesIsRefused(t *testing.T) {
	if _, err := LoadEngine(PolicyBundleDoc{}); err == nil {
		t.Fatal("a bundle with no policies loaded. It authorizes nothing while reading as though it " +
			"governs something, which is the same argument the per-entry guard already makes")
	}

	// The real shape it was found in: valid YAML, right apiVersion, wrong key.
	var doc PolicyBundleDoc
	raw := []byte("apiVersion: harness.ai/v1\nkind: PolicyBundle\nstatements:\n  - id: allow-x\n    effect: permit\n")
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if len(doc.Policies) != 0 {
		t.Fatalf("the decoder picked up %d policies from a `statements:` key — this test's premise is "+
			"that it silently picks up none", len(doc.Policies))
	}
	if _, err := LoadEngine(doc); err == nil {
		t.Fatal("the misspelled bundle loaded as an empty engine")
	} else if !strings.Contains(err.Error(), "policies:") {
		// The error has to name the likely cause. "This bundle is empty" sends an operator looking at
		// their Cedar; naming the key sends them to the line that is actually wrong.
		t.Errorf("the error does not mention the `policies:` key: %v", err)
	}
}
