package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/auth"
	"github.com/aeon-ai/aeon/go/internal/runcontroller"
)

// TestAnotherTenantsRunIsIndistinguishableFromOneThatDoesNotExist is VRT-AEON-005 T-6, and it walks
// EVERY by-id route rather than sampling one.
//
// That exhaustiveness is the enforcement. A store handle can be made impossible to build without a
// tenant and the compiler then answers once per type (GOV-001c); a path value cannot, so the only
// thing standing between this and an isolation implemented route by route — which is what slice 2
// found in the Memory Store, five routes checking nothing — is a test that enumerates the routes. If
// a route is added without the gate, this test is where it shows up, so the list below is part of
// the feature and not a convenience.
func TestAnotherTenantsRunIsIndistinguishableFromOneThatDoesNotExist(t *testing.T) {
	temporalClient := testTemporalClient(t)
	controller := runcontroller.New(temporalClient, "tenancy-test-"+randSuffix(t))

	mux := http.NewServeMux()
	(&RunControllerHandlers{Controller: controller}).Register(mux)

	// Two servers over the SAME handlers, differing only in which tenant their caller belongs to.
	// Same process, same controller, same Temporal: the only variable is the credential.
	owner := httptest.NewServer(authWrapTenant(t, mux, "tenant-owner"))
	t.Cleanup(owner.Close)
	stranger := httptest.NewServer(authWrapTenant(t, mux, "tenant-stranger"))
	t.Cleanup(stranger.Close)

	// Started through the API so the tenant reaches the memo the way it does in production — not
	// planted by calling Controller.Start directly, which would skip the half under test.
	runID := "tenancy-" + randSuffix(t)
	body, _ := json.Marshal(map[string]any{
		"run_id": runID,
		"graph":  map[string]any{"id": "root", "kind": "sequential", "children": []any{}},
	})
	resp := postJSONAuthed(t, owner.URL+"/runs", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var parsed map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&parsed)
		t.Fatalf("starting the run = %d: %v", resp.StatusCode, parsed)
	}

	// NEGATIVE CONTROL FIRST. If the owner cannot read its own run, every 404 below means nothing.
	//
	// THIS IS WHERE THE TEST'S ~60 SECONDS GO, and it is worth knowing before somebody "optimises"
	// it away: GET /runs/{id} calls Status, which queries the workflow, and there is no worker on
	// this test's task queue — so the query waits out Temporal's timeout before Status falls back to
	// what Describe already told it. The gate itself is instant (every subtest below is 0.00s). The
	// alternative was to drop the control, and a suite of 404s with nothing proving a 200 is
	// possible is exactly the shape that passes when everything is broken.
	if code := get(t, owner, "/runs/"+runID); code != http.StatusOK {
		t.Fatalf("the owner's own GET = %d, want 200 — the test below would prove nothing", code)
	}

	routes := []struct {
		method, path string
		body         []byte
	}{
		{"GET", "/runs/" + runID, nil},
		{"GET", "/runs/" + runID + "/stream", nil},
		{"POST", "/runs/" + runID + "/cancel", []byte("{}")},
		{"POST", "/runs/" + runID + "/pause", []byte("{}")},
		{"POST", "/runs/" + runID + "/resume", []byte("{}")},
		{"POST", "/runs/" + runID + "/approve", []byte(`{"tool_call_hash":"x"}`)},
		{"POST", "/runs/" + runID + "/reject", []byte(`{"tool_call_hash":"x"}`)},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			code := do(t, stranger, rt.method, rt.path, rt.body)
			if code == http.StatusForbidden {
				t.Fatalf("status = 403: a forbidden answer CONFIRMS the run exists. Across a tenant " +
					"boundary it has to be indistinguishable from a run that never existed (T-7)")
			}
			if code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", code)
			}
		})
	}

	// And the run is untouched: a refusal that cancelled or paused on its way out would be worse
	// than a leak.
	if code := get(t, owner, "/runs/"+runID); code != http.StatusOK {
		t.Errorf("after the stranger's attempts the owner's run answers %d — something took effect", code)
	}
}

func get(t *testing.T, srv *httptest.Server, path string) int {
	t.Helper()
	return do(t, srv, http.MethodGet, path, nil)
}

func do(t *testing.T, srv *httptest.Server, method, path string, body []byte) int {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(authorize(req))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// authWrapTenant is authWrap with the caller's tenant chosen by the test, which is the one thing
// these two servers have to differ in.
func authWrapTenant(t *testing.T, mux *http.ServeMux, tenant string, actsAs ...string) http.Handler {
	t.Helper()
	a, err := auth.Load(auth.CallerBundleDoc{Kind: "CallerBundle", Callers: []auth.Caller{{
		ID:          "test-caller-" + tenant,
		Kind:        auth.KindService,
		Tenant:      tenant,
		TokenSHA256: auth.HashToken(testCallerToken),
		MayActAs:    actsAs,
		MayApprove:  true,
	}}})
	if err != nil {
		t.Fatalf("building the test authenticator for %s: %v", tenant, err)
	}
	return auth.Require(a)(mux)
}
