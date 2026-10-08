package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/auth"
	"gopkg.in/yaml.v3"
)

// testCallerToken is the credential every test in this package presents. Its hash is built here, not
// read from a file, so these tests do not depend on the development bundle's contents.
const testCallerToken = "test-caller-token-for-this-package"

// authedServer wraps mux the way httpserver.New does and serves it, with one caller allowed to act as
// the agents named.
//
// WHY EACH TEST NAMES ITS OWN `actsAs` RATHER THAN SHARING A PERMISSIVE CALLER. After SEC-005 a caller
// may only present an agent its entry lists, so a test whose subject is POLICY — "Cedar denies this
// agent this tool" — has to be able to claim that agent, or the refusal it asserts comes from
// authentication instead and the test passes while proving something else. The caller is therefore as
// permissive as that test's own subject requires and no more; the restrictive properties are asserted
// in TestTheDeploymentRefusesCallersItCannotIdentify, against the real committed bundle.
func authedServer(t *testing.T, mux *http.ServeMux, actsAs ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(authWrap(t, mux, actsAs...))
	t.Cleanup(srv.Close)
	return srv
}

// authWrap is authedServer's middleware half, for tests that build their own httptest.Server.
func authWrap(t *testing.T, mux *http.ServeMux, actsAs ...string) http.Handler {
	t.Helper()
	a, err := auth.Load(auth.CallerBundleDoc{Kind: "CallerBundle", Callers: []auth.Caller{{
		ID:          "test-caller",
		Kind:        auth.KindService,
		Tenant:      "default",
		TokenSHA256: auth.HashToken(testCallerToken),
		MayActAs:    actsAs,
		MayApprove:  true,
	}}})
	if err != nil {
		t.Fatalf("building the test authenticator: %v", err)
	}
	return auth.Require(a)(mux)
}

// shippedPlusTestCallers authenticates BOTH the shipped development bundle and this package's own test
// caller.
//
// WHY BOTH, and it is not convenience. TestEnforcementSeamLatencyByDeploymentShape compares a local
// in-process seam against the real aeon-toolgw container: the remote shape verifies against the shipped
// bundle, so the local one must accept that bundle's token or the comparison measures two different
// authentication setups along with the deployment difference. Every other test in this package needs a
// caller with its own `mayActAs`, which the shipped bundle does not have. One authenticator that knows
// both is the only arrangement where neither test has to pretend.
func shippedPlusTestCallers(t *testing.T, actsAs ...string) *auth.Authenticator {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(repoPolicyBundlePath(t)), "callers.yaml"))
	if err != nil {
		t.Fatalf("reading the shipped caller bundle: %v", err)
	}
	var doc auth.CallerBundleDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the shipped caller bundle: %v", err)
	}
	doc.Callers = append(doc.Callers, auth.Caller{
		ID:          "test-caller",
		Kind:        auth.KindService,
		Tenant:      "default",
		TokenSHA256: auth.HashToken(testCallerToken),
		MayActAs:    actsAs,
		MayApprove:  true,
	})
	a, err := auth.Load(doc)
	if err != nil {
		t.Fatalf("loading the combined caller bundle: %v", err)
	}
	return a
}

// authorize puts this package's test credential on a request. Called by every POST/GET helper here, so
// a new helper that forgets it fails loudly with a 401 rather than quietly skipping the guard.
func authorize(req *http.Request) *http.Request {
	req.Header.Set("Authorization", "Bearer "+testCallerToken)
	return req
}

// postJSONAuthed is the shared shape of this package's POST helpers: a JSON body with the credential.
func postJSONAuthed(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, url, reader)
	if err != nil {
		t.Fatalf("building POST %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(authorize(req))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// getAuthed is the GET half. Added after the POST half, because the first version of this harness
// authorized only POSTs and the GETs came back 401 — which read as "the journal endpoint is broken"
// rather than "this helper forgot the credential".
func getAuthed(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building GET %s: %v", url, err)
	}
	resp, err := http.DefaultClient.Do(authorize(req))
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}
