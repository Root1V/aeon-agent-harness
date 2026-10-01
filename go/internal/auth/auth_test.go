package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCallerBundleRefusesWhatItCannotMean is SEC-005's unit half: every way a caller bundle can be
// wrong is refused at load rather than carried into a running service.
//
// WHY LOADING IS THE PLACE. A security config that half-loads is worse than one that does not load:
// the operator sees a started service and concludes it is configured. Each case below is a mistake
// somebody will actually make, and each one has a specific way of being silently wrong if accepted.
func TestCallerBundleRefusesWhatItCannotMean(t *testing.T) {
	good := Caller{ID: "worker", Kind: KindService, TokenSHA256: HashToken("t1"), MayActAs: []string{"a@1"}}

	cases := []struct {
		name    string
		doc     CallerBundleDoc
		wantErr string
		why     string
	}{
		{
			name:    "no callers at all",
			doc:     CallerBundleDoc{Kind: "CallerBundle"},
			wantErr: "declares no callers",
			why:     "a bundle nobody can pass gets 'fixed' by removing the requirement, not by filling it in",
		},
		{
			name:    "wrong kind of document",
			doc:     CallerBundleDoc{Kind: "PolicyBundle", Callers: []Caller{good}},
			wantErr: "kind CallerBundle",
			why:     "the two bundles live side by side and are both YAML; pointing at the wrong one must say so",
		},
		{
			name:    "a caller with no token",
			doc:     CallerBundleDoc{Callers: []Caller{{ID: "x", Kind: KindService}}},
			wantErr: "64-character hex",
			why:     "an empty hash would match the hash of nothing, and `Bearer ` is a request away",
		},
		{
			name:    "a token hash that is not a hash",
			doc:     CallerBundleDoc{Callers: []Caller{{ID: "x", Kind: KindService, TokenSHA256: strings.Repeat("z", 64)}}},
			wantErr: "non-hex",
			why:     "a pasted token instead of its hash is the most likely mistake here, and it is 64 chars often enough",
		},
		{
			name:    "an unknown kind",
			doc:     CallerBundleDoc{Callers: []Caller{{ID: "x", Kind: "robot", TokenSHA256: HashToken("t")}}},
			wantErr: "want one of service|human|external",
			why:     "the kind is written into the audit line of every approval; an unvalidated one makes that line unaggregatable",
		},
		{
			name: "two callers with the same id",
			doc: CallerBundleDoc{Callers: []Caller{
				good,
				{ID: "worker", Kind: KindHuman, TokenSHA256: HashToken("t2"), MayApprove: true},
			}},
			wantErr: "duplicate caller id",
			why:     "whichever entry won would decide both what is permitted and what the journal records",
		},
		{
			name: "two callers sharing a token",
			doc: CallerBundleDoc{Callers: []Caller{
				good,
				{ID: "other", Kind: KindHuman, TokenSHA256: HashToken("t1"), MayApprove: true},
			}},
			wantErr: "share a token",
			why:     "that is not two identities, it is one identity with two names — and the audit line gets whichever the map returned",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.doc)
			if err == nil {
				t.Fatalf("the bundle loaded. %s", tc.why)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestRequireRefusesEveryCredentialThatIsNotOne is the middleware's own half.
func TestRequireRefusesEveryCredentialThatIsNotOne(t *testing.T) {
	a, err := Load(CallerBundleDoc{Callers: []Caller{
		{ID: "worker", Kind: KindService, TokenSHA256: HashToken("right-token"), MayActAs: []string{"a@1"}},
	}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var sawCaller string
	guarded := Require(a)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := CallerFrom(r.Context())
		if !ok {
			t.Error("the handler ran with no caller in its context — Require let a request past without identifying it")
		}
		sawCaller = c.ID
		w.WriteHeader(http.StatusOK)
	}))

	for _, tc := range []struct {
		name, header string
		want         int
	}{
		{"no header at all", "", http.StatusUnauthorized},
		{"the right token", "Bearer right-token", http.StatusOK},
		{"a wrong token", "Bearer wrong-token", http.StatusUnauthorized},
		{"the right token with no scheme", "right-token", http.StatusUnauthorized},
		{"an empty bearer", "Bearer ", http.StatusUnauthorized},
		{"the hash instead of the token", "Bearer " + HashToken("right-token"), http.StatusUnauthorized},
		// Lowercase scheme: RFC 7235 says the scheme is case-insensitive, and a client library that
		// sends `bearer` must not be told its token is wrong.
		{"a lowercase scheme", "bearer right-token", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sawCaller = ""
			req := httptest.NewRequest(http.MethodGet, "/anything", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			guarded.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusOK && sawCaller != "worker" {
				t.Fatalf("the handler saw caller %q, want \"worker\"", sawCaller)
			}
			if tc.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatalf("a 401 with no WWW-Authenticate tells a client it failed and not how to succeed")
			}
		})
	}
}

// TestApprovalIsNotImpliedByExecution is the property the whole feature exists for.
//
// A worker that may act as an agent must not thereby be able to approve. If these two permissions were
// one, the process that gets blocked on an approval would hold the credential that answers it, and the
// gate on an irreversible action would be a formality that writes an audit entry.
func TestApprovalIsNotImpliedByExecution(t *testing.T) {
	a, err := Load(CallerBundleDoc{Callers: []Caller{
		{ID: "worker", Kind: KindService, TokenSHA256: HashToken("w"), MayActAs: []string{"deep-research-general@0.1.0"}},
		{ID: "operator", Kind: KindHuman, TokenSHA256: HashToken("o"), MayApprove: true},
	}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	worker, err := a.Authenticate("w")
	if err != nil {
		t.Fatalf("authenticating the worker: %v", err)
	}
	if !worker.ActsAs("deep-research-general@0.1.0") {
		t.Fatalf("the worker cannot act as the agent its entry lists")
	}
	if worker.MayApprove {
		t.Fatalf("the worker may approve. It is the process that gets BLOCKED on an approval, so this " +
			"credential would let the run approve its own irreversible call")
	}

	operator, err := a.Authenticate("o")
	if err != nil {
		t.Fatalf("authenticating the operator: %v", err)
	}
	if !operator.MayApprove {
		t.Fatalf("the operator may not approve")
	}
	// And the other direction, which matters as much: deciding an approval is not a licence to execute.
	if operator.ActsAs("deep-research-general@0.1.0") {
		t.Fatalf("the operator can act as an agent without its entry listing one — an empty mayActAs must " +
			"permit nothing, not everything")
	}
}

// TestActsAsHasNoWildcard pins the absence of a convenience that would undo the feature.
func TestActsAsHasNoWildcard(t *testing.T) {
	c := Caller{ID: "x", Kind: KindService, MayActAs: []string{"*"}}
	if c.ActsAs("anything@1.0.0") {
		t.Fatalf("`*` in mayActAs acted as a wildcard. It must be a literal: a wildcard is one config " +
			"edit away from restoring the hole SEC-005 closed, and the deployment would look configured")
	}
	if !c.ActsAs("*") {
		t.Fatalf("`*` stopped matching itself, which would be a different kind of surprise")
	}
}

// TestAServiceWithNoBundleRefusesToStart pins the fail-closed decision this whole feature rests on.
//
// The two alternatives for a missing bundle are both worse than not starting: serve everything
// unauthenticated — the state SEC-005 exists to end, and it would log a warning nobody reads — or
// serve nothing while answering health probes, which looks like a working deployment. `fatal` is a
// package variable precisely so this can be asserted without spawning a process.
func TestAServiceWithNoBundleRefusesToStart(t *testing.T) {
	original := fatal
	t.Cleanup(func() { fatal = original })

	var got string
	fatal = func(format string, args ...any) {
		got = fmt.Sprintf(format, args...)
		panic(sentinelFatal)
	}

	t.Setenv(CallersPathEnv, "")
	func() {
		defer func() {
			if r := recover(); r != sentinelFatal {
				t.Fatalf("MustLoadFromEnv returned instead of refusing (recovered %v)", r)
			}
		}()
		MustLoadFromEnv("aeon-test")
		t.Fatal("MustLoadFromEnv returned an Authenticator with no bundle configured")
	}()

	for _, want := range []string{CallersPathEnv, "callers.yaml"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the refusal does not mention %q, so the operator who has not configured this yet "+
				"has to go looking: %q", want, got)
		}
	}
}

const sentinelFatal = "auth-test-fatal"
