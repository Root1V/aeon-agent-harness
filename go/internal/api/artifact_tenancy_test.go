package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// GOV-001g's acceptance test: one tenant cannot read another's artifacts.
//
// THE DEFECT, MEASURED BEFORE THE FIX on 2026-10-08, through this same gateway with real Cedar
// bundles. An artifact id is `art_ + uuid5(FIXED_NAMESPACE, "<run_id>/<name>").hex` — deterministic,
// with the namespace a constant in this repository and the name a literal ("report.md" for every
// Deep Research run) — and every tenant's artifacts lived in ONE flat directory. So a caller in
// tenant-a that had seen a tenant-b run id (they travel in X-Aeon-Run-Id, in POST /runs, in traces,
// in the cost ledger) could compute the id of B's finished report and ask for it:
//
//	{"allowed":true,"policy_id":"allow-artifact-read",
//	 "result":{"content":"<tenant B's report>","bytes":28,"status":"executed"}}
//
// The tool's own doc comment said "a run can fetch what it produced and cannot enumerate what anyone
// else did", and that was true about ENUMERATION and false about everything else.
//
// WHY THIS IS CONTAINMENT AND NOT A CHECK. The tool is handed a resolver and gets a PER-TENANT
// os.Root, so B's artifact is not something A is refused — it is outside the root A's call can
// address. There is no predicate to forget, which is the property engineFor already has for bundles.
// A test of a check would have to trust that the check is reached on every path; this one cannot be
// bypassed by a path that forgets it, because there is no path.
func TestOneTenantCannotReadAnothersArtifacts(t *testing.T) {
	const (
		agentRef = "artifact-tenancy@0.1.0"
		tenantA  = "tenant-a"
		tenantB  = "tenant-b"
		// The real derivation: art_ + uuid5(6f1a5d0e-…, "run-of-b-0001/report.md").hex, computed with
		// python/aeon_worker/activities/artifact_activities.artifact_id. Written as a literal so this
		// test fails if that derivation ever changes without the stores being migrated — the id IS the
		// contract between the writer and the reader.
		bReportID = "art_e09d8e11d70655e6ad3dc8ac84f18e7e"
		bContent  = "tenant B's finished report"
	)

	root := t.TempDir()
	// The store as the writer produces it since GOV-001g: <root>/<tenant>/<id>.
	for tenant, content := range map[string]string{tenantB: bContent, tenantA: "tenant A's own report"} {
		if err := os.MkdirAll(filepath.Join(root, tenant), 0o755); err != nil {
			t.Fatal(err)
		}
		id := bReportID
		if tenant == tenantA {
			id = "art_" + "a" + bReportID[len("art_")+1:]
		}
		if err := os.WriteFile(filepath.Join(root, tenant, id), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	srv := artifactGatewayServer(t, root, agentRef, []string{tenantA, tenantB})

	t.Run("B reads its own report", func(t *testing.T) {
		// First, because without it the refusal below proves nothing: a gateway that served nothing
		// to anybody would pass the cross-tenant assertion.
		status, body := readArtifact(t, srv, tenantB, agentRef, bReportID)
		if status != http.StatusOK {
			t.Fatalf("B could not read its own artifact: status=%d body=%v", status, body)
		}
		result, _ := body["result"].(map[string]any)
		if result["content"] != bContent {
			t.Fatalf("B read %v, want its own report", result["content"])
		}
	})

	t.Run("A cannot read B's report, and the answer does not confirm it exists", func(t *testing.T) {
		status, body := readArtifact(t, srv, tenantA, agentRef, bReportID)
		if status == http.StatusOK {
			result, _ := body["result"].(map[string]any)
			t.Fatalf("tenant A read tenant B's artifact: %v", result["content"])
		}
		// T-7: across a tenant boundary the answer must not distinguish "not yours" from "not there",
		// and here it cannot — the file is outside the root the call can address, so the only thing
		// the gateway knows is that it is absent. That is the shape T-7 asks for, obtained by
		// construction rather than by remembering to answer 404 instead of 403.
		raw, _ := json.Marshal(body)
		if bytes.Contains(raw, []byte(bContent)) {
			t.Fatalf("B's content leaked into the refusal: %s", raw)
		}
		errMsg, _ := body["error"].(string)
		if regexp.MustCompile(`(?i)forbidden|not your|another tenant`).MatchString(errMsg) {
			t.Errorf("the refusal says the artifact belongs to somebody else, which confirms it "+
				"exists: %q", errMsg)
		}
	})

	t.Run("a call with no tenant is refused, not served from the root", func(t *testing.T) {
		// The INT-003 path: an external MCP caller shares one Cedar principal and is not a run, so it
		// has no tenant. Serving it from <root> directly would be the old flat directory, restored by
		// the one caller that has no tenant to scope it to.
		status, body := readArtifact(t, srv, "", agentRef, bReportID)
		if status == http.StatusOK {
			t.Fatalf("an untenanted call was served an artifact: %v", body["result"])
		}
		if errMsg, _ := body["error"].(string); errMsg == "" {
			t.Error("the refusal carries no message saying why there is no store to read from")
		}
	})

	t.Run("a tenant that is a path is refused", func(t *testing.T) {
		// effectiveTenant already validates the header, so this is belt and braces — and it is here
		// because the tenant became a PATH COMPONENT in this change, which is a new way for a name to
		// be dangerous. `..` must be a rejected name and never a traversal.
		status, body := readArtifactRaw(t, srv, "../..", agentRef, bReportID)
		if status == http.StatusOK {
			t.Fatalf("a traversal in the tenant header was served: %v", body["result"])
		}
	})
}

// artifactGatewayServer mounts the real artifact.read over a per-tenant resolver, exactly as
// cmd/aeon-toolgw does.
func artifactGatewayServer(t *testing.T, root, agentRef string, tenants []string) *httptest.Server {
	t.Helper()
	tenantDir := regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	executor := toolexec.NewExecutor()
	toolexec.RegisterArtifactReadTool(executor, func(tenant string) (*os.Root, error) {
		if !tenantDir.MatchString(tenant) {
			return nil, fmt.Errorf("%q is not a tenant name", tenant)
		}
		return os.OpenRoot(filepath.Join(root, tenant))
	}, root)

	dir := t.TempDir()
	for _, tenant := range tenants {
		bundle := "apiVersion: harness.ai/v1\nkind: PolicyBundle\ncedarVersion: \"4.0\"\npolicies:\n" +
			"  - id: allow-artifact-read\n    effect: permit\n    cedarSource: |\n" +
			"      permit(\n        principal == Agent::\"" + agentRef + "\",\n        action,\n" +
			"        resource\n      ) when {\n        resource is Tool &&\n" +
			"        resource.name == \"artifact.read\"\n      };\n"
		if err := os.WriteFile(filepath.Join(dir, tenant+".yaml"), []byte(bundle), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	set, err := policy.LoadSetFromDir(dir)
	if err != nil {
		t.Fatalf("LoadSetFromDir: %v", err)
	}

	mux := http.NewServeMux()
	(&ToolGatewayHandlers{Policy: set, Executor: executor}).Register(mux)
	srv := httptest.NewServer(workerAuthWrap(t, mux, "ops", tenants, agentRef))
	t.Cleanup(srv.Close)
	return srv
}

func readArtifact(t *testing.T, srv *httptest.Server, runTenant, agentRef, artifactID string) (int, map[string]any) {
	t.Helper()
	return readArtifactRaw(t, srv, runTenant, agentRef, artifactID)
}

func readArtifactRaw(t *testing.T, srv *httptest.Server, runTenant, agentRef, artifactID string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(toolCallRequest{
		AgentManifestRef: agentRef, ToolName: "artifact.read",
		Args: map[string]any{"artifact_id": artifactID},
	})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/execute", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if runTenant != "" {
		req.Header.Set(RunTenantHeader, runTenant)
	}
	resp, err := http.DefaultClient.Do(authorize(req))
	if err != nil {
		t.Fatalf("POST /execute: %v", err)
	}
	defer resp.Body.Close()
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}
