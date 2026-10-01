package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/aeon-ai/aeon/go/internal/auth"
	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/store"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// newEnforcementSeamServer is the real enforcement seam: the real Cedar engine over the checked-in bundle,
// reached over HTTP, with an executor that FAILS THE TEST if a refused call reaches it.
//
// Over HTTP and not as a Go call, because that is what the seam IS — synchronous, deniable, and outside the
// loop's process. A test calling policy.Engine directly would verify the decision and nothing about whether
// the loop can act on it, which is the half INT-010 is about.
func newEnforcementSeamServer(t *testing.T, checkpointer bool) *httptest.Server {
	t.Helper()
	raw, err := os.ReadFile(repoPolicyBundlePath(t))
	if err != nil {
		t.Fatalf("reading policy bundle: %v", err)
	}
	var doc policy.PolicyBundleDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing policy bundle: %v", err)
	}
	engine, err := policy.LoadEngine(doc)
	if err != nil {
		t.Fatalf("loading Cedar engine: %v", err)
	}

	executor := toolexec.NewExecutor()
	executor.Register("search.web", func(args map[string]any) (map[string]any, error) {
		return map[string]any{"status": "executed"}, nil
	})
	for _, refused := range []string{"shell.exec", "external.write.database", "artifact.write"} {
		name := refused
		executor.Register(name, func(map[string]any) (map[string]any, error) {
			t.Errorf("the executor ran %q, which policy refuses — the seam reported a decision it did not enforce", name)
			return map[string]any{}, nil
		})
	}

	handlers := &ToolGatewayHandlers{Policy: engine, Executor: executor}
	mux := http.NewServeMux()
	if checkpointer {
		s, err := store.Connect(context.Background(), memoryTestDSN(t))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(s.Close)
		handlers.Checkpointer = s.Checkpointer()
		// The journal is mounted alongside so the test reads records back the way the framework does — over
		// HTTP, which is the only way the Python loop can reach them.
		(&CheckpointHandlers{Checkpointer: s.Checkpointer()}).Register(mux)
	}
	handlers.Register(mux)
	// The SHIPPED bundle on the local shape too, not this package's own test caller. The remote shape of
	// this comparison is the real aeon-toolgw container, which verifies against the shipped bundle — so
	// using a different one locally would mean the two shapes authenticate differently, and the latency
	// comparison would be measuring that difference along with the deployment one.
	srv := httptest.NewServer(auth.Require(shippedPlusTestCallers(t, "deep-research-general@0.1.0"))(mux))
	t.Cleanup(srv.Close)
	return srv
}

type seamResponse struct {
	Allowed             bool   `json:"allowed"`
	Disposition         string `json:"disposition"`
	DispositionDeclared bool   `json:"disposition_declared"`
	PolicyID            string `json:"policy_id"`
	Outcome             string `json:"outcome"`
	Journalled          bool   `json:"journalled"`
}

func askSeam(t *testing.T, srv *httptest.Server, path, agent, tool string, extra map[string]string) (int, seamResponse) {
	t.Helper()
	body := map[string]any{"agent_manifest_ref": agent, "tool_name": tool, "args": map[string]any{}}
	for k, v := range extra {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	resp := postJSONAuthed(t, srv.URL+path, raw)
	defer resp.Body.Close()
	var parsed seamResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decoding %s response: %v", path, err)
	}
	return resp.StatusCode, parsed
}

// TestEnforcementSeamDeniesWithDisposition is INT-010's acceptance test.
//
// THE POINT: the seam returns a TYPE WITH A DISPOSITION, not a boolean. `allowed: false` tells a loop the
// call failed and leaves it to choose between three different behaviours it cannot pick between — try
// something else, end the run, or suspend and ask a person. Synaptum's loop acts on that choice, so the
// vocabulary is theirs; this test pins our end of it.
//
// The four values are allow · deny_step · terminate_run · require_approval, published by Synaptum on
// 2026-09-27. An earlier version of this row listed three I had invented by deduction; three happened to be
// right, which was luck, and the row sat in BLOCKED rather than being built on a guess.
func TestEnforcementSeamDeniesWithDisposition(t *testing.T) {
	const agent = "deep-research-general@0.1.0"

	t.Run("every decision carries a disposition, over HTTP, on both endpoints", func(t *testing.T) {
		srv := newEnforcementSeamServer(t, false)

		for _, tc := range []struct {
			tool        string
			wantStatus  int
			disposition string
			declared    bool
		}{
			{"search.web", http.StatusOK, "allow", false},
			{"shell.exec", http.StatusForbidden, "terminate_run", true},
			{"external.write.database", http.StatusForbidden, "deny_step", true},
			{"artifact.write", http.StatusForbidden, "require_approval", true},
			{"nothing.permits.this", http.StatusForbidden, "deny_step", false},
		} {
			t.Run(tc.tool, func(t *testing.T) {
				// /check-policy: the advisory read a loop makes before committing to a step.
				status, checked := askSeam(t, srv, "/check-policy", agent, tc.tool, nil)
				if status != http.StatusOK {
					t.Fatalf("/check-policy status = %d, want 200 (the check itself succeeds even when the answer is no)", status)
				}
				if checked.Disposition != tc.disposition {
					t.Errorf("/check-policy disposition = %q, want %q", checked.Disposition, tc.disposition)
				}
				if checked.DispositionDeclared != tc.declared {
					t.Errorf("/check-policy declared = %v, want %v — a considered deny_step and one nobody thought about are different facts about the bundle",
						checked.DispositionDeclared, tc.declared)
				}

				// /execute: the enforcing path. The disposition has to be on BOTH, or a loop that skipped the
				// advisory check — which it is entitled to do — would get a bare refusal.
				status, executed := askSeam(t, srv, "/execute", agent, tc.tool, nil)
				if status != tc.wantStatus {
					t.Fatalf("/execute status = %d, want %d", status, tc.wantStatus)
				}
				if tc.wantStatus == http.StatusForbidden && executed.Disposition != tc.disposition {
					t.Errorf("/execute disposition = %q, want %q", executed.Disposition, tc.disposition)
				}
			})
		}
	})

	t.Run("require_approval is refused execution but is NOT a denial", func(t *testing.T) {
		// The distinction the whole design turns on. A permit that requires approval means Cedar said yes
		// and the seam is withholding execution until a person decides. So: refused now, and NOT recorded as
		// denied — writing a denial would log a refusal nobody made, and the run would read as closed while
		// it is merely suspended.
		srv := newEnforcementSeamServer(t, true)
		runID := newRunID("disposition-approval")

		status, body := askSeam(t, srv, "/execute", agent, "artifact.write",
			map[string]string{"run_id": runID, "step_id": "n0"})
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 — `allowed` means may-execute-NOW, and nobody has approved yet", status)
		}
		if body.Disposition != "require_approval" {
			t.Fatalf("disposition = %q", body.Disposition)
		}
		if body.Journalled {
			t.Error("a require_approval step was journalled as an outcome — it has none yet, and recording one would make a suspended run look finished")
		}
		if body.Outcome != "" {
			t.Errorf("outcome = %q, want empty", body.Outcome)
		}

		// And the journal agrees: the step has NO outcome, which is what leaves it resumable.
		state := loadRunState(t, srv, runID)
		if _, _, ok := state.StepOutcome("n0"); ok {
			t.Error("the journal holds an outcome for a step that is waiting for a person")
		}
	})

	t.Run("a denied step IS recorded, and terminate_run records a denial too", func(t *testing.T) {
		// The counterpart. Both refusals are real outcomes, so both are journalled (INT-011) — only
		// require_approval is not.
		srv := newEnforcementSeamServer(t, true)
		for _, tc := range []struct{ tool, disposition string }{
			{"shell.exec", "terminate_run"},
			{"external.write.database", "deny_step"},
		} {
			t.Run(tc.tool, func(t *testing.T) {
				runID := newRunID("disposition-denied")
				_, body := askSeam(t, srv, "/execute", agent, tc.tool,
					map[string]string{"run_id": runID, "step_id": "n0"})
				if body.Disposition != tc.disposition {
					t.Fatalf("disposition = %q, want %q", body.Disposition, tc.disposition)
				}
				if !body.Journalled {
					t.Fatalf("the refusal was not journalled")
				}
				outcome, _, ok := loadRunState(t, srv, runID).StepOutcome("n0")
				if !ok || !outcome.Denied() {
					t.Errorf("journal outcome = %q (found=%v), want a denied one", outcome, ok)
				}
			})
		}
	})

	t.Run("the disposition is not a boolean wearing a name", func(t *testing.T) {
		// The assertion that would fail if someone "simplified" the field back to a two-valued thing. Four
		// distinct dispositions must actually appear across the bundle, or the type is decoration: a seam
		// that only ever answers allow/deny_step has the same information content as the boolean it replaced.
		srv := newEnforcementSeamServer(t, false)
		seen := map[string]bool{}
		for _, tool := range []string{"search.web", "shell.exec", "external.write.database", "artifact.write"} {
			_, body := askSeam(t, srv, "/check-policy", agent, tool, nil)
			seen[body.Disposition] = true
		}
		var got []string
		for d := range seen {
			got = append(got, d)
		}
		sort.Strings(got)
		if len(got) != 4 {
			t.Errorf("the seam produced %d distinct disposition(s) across the bundle (%v), want all four — fewer means the field carries no more than the boolean it replaced", len(got), got)
		}
	})
}

// TestEnforcementSeamLatencyByDeploymentShape is INT-010's measurement half: a remote HTTP gateway against a
// local egress proxy, including the streaming path.
//
// It exists because the seam is SYNCHRONOUS and on the path of every tool call, so where it is deployed is a
// latency decision Synaptum has to make with numbers rather than intuition. It reports and does not assert a
// threshold: a number that fails a build on shared CI hardware teaches people to widen the threshold, and
// then it measures nothing. What it DOES assert is that both shapes were actually reached, so a missing
// gateway cannot be reported as a fast one.
func TestEnforcementSeamLatencyByDeploymentShape(t *testing.T) {
	remote := os.Getenv("AEON_TEST_TOOLGW_ADDR")
	if remote == "" {
		t.Skip("AEON_TEST_TOOLGW_ADDR not set — skipping the seam latency measurement (needs the real aeon-toolgw container)")
	}

	// The local shape: the same handler, same real Cedar engine, same checked-in bundle, reached over
	// loopback. Same code on both sides is what makes the comparison about DEPLOYMENT and not about two
	// implementations.
	local := newEnforcementSeamServer(t, false)

	const agent = "deep-research-general@0.1.0"
	measure := func(t *testing.T, label, url, tool string) time.Duration {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"agent_manifest_ref": agent, "tool_name": tool, "args": map[string]any{},
		})
		client := &http.Client{Timeout: 10 * time.Second}

		// Warmed first, and the warm-up is not cosmetic: the first call pays TCP setup and, on the remote
		// shape, DNS. Reporting that as the per-call cost of enforcement would overstate it by an order of
		// magnitude and send Synaptum to the wrong deployment.
		// SEC-005: both shapes authenticate now, and this helper is where that was missing. The local
		// shape is wrapped like the binary and the remote one IS the binary, so a call with no credential
		// gets a 401 from both — and this test had never run in any target, so nothing said so until
		// toolgw was added to the Go integration target.
		post := func() (*http.Response, error) {
			req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+seamCallerToken())
			return client.Do(req)
		}

		for i := 0; i < 5; i++ {
			resp, err := post()
			if err != nil {
				t.Fatalf("%s: warm-up: %v", label, err)
			}
			resp.Body.Close()
		}

		const n = 60
		samples := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			start := time.Now()
			resp, err := post()
			if err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200 — a shape that did not answer must not be reported as a fast one", label, resp.StatusCode)
			}
			samples = append(samples, time.Since(start))
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		// The MEDIAN and p95, not the mean: one scheduler hiccup on shared CI moves a mean and tells you
		// nothing about what a call normally costs.
		median, p95 := samples[len(samples)/2], samples[(len(samples)*95)/100]
		t.Logf("%-38s median=%-10v p95=%-10v n=%d", label, median.Round(time.Microsecond), p95.Round(time.Microsecond), n)
		return median
	}

	remoteURL := "http://" + remote + "/check-policy"
	localMedian := measure(t, "local egress proxy (loopback)", local.URL+"/check-policy", "search.web")
	remoteMedian := measure(t, "remote HTTP gateway (container net)", remoteURL, "search.web")

	// A denied call is measured too, because it takes a different path through the handler — and the
	// interesting question for a loop is whether refusing costs more than allowing, since a refusal is the
	// case a runaway agent produces repeatedly.
	measure(t, "remote HTTP gateway, DENIED call", remoteURL, "shell.exec")

	t.Logf("enforcement hop cost: the remote shape adds %v per tool call over the local one",
		(remoteMedian - localMedian).Round(time.Microsecond))
	t.Logf("what this means for the loop: enforcement is per TOOL CALL, not per token, so a run making %d tool calls pays %v of it in the remote shape",
		20, (20 * remoteMedian).Round(time.Millisecond))

	// The streaming half. Enforcement sits on the tool path and a model call streams, so the question worth
	// answering is whether the hop delays the FIRST TOKEN — which is what a user perceives — or only the
	// total. Measured against the real streaming endpoint if one is reachable.
	if modelgw := os.Getenv("AEON_TEST_MODELGW_ADDR"); modelgw != "" {
		ttfb := measureFirstByte(t, modelgw)
		if ttfb > 0 {
			// THE CONCLUSION THIS MEASUREMENT EXISTS FOR, stated here rather than left for a reader to
			// compute: the enforcement hop is dwarfed by model latency. So the seam's deployment shape is
			// not a latency decision at all, and the remote gateway costs nothing a person could perceive
			// against a local sidecar — it should be picked on operational grounds, one process to run and
			// patch instead of one per workload.
			//
			// THE RATIO IS ONE SAMPLE AND MOVES A LOT, which is why the number is printed with that said out
			// loud. Observed 2.8s cold and 231ms warm on the same deployment minutes apart — a 10x spread
			// from prompt caching alone. That makes it poor evidence for a precise ratio and overwhelming
			// evidence for the only claim being made: three to four orders of magnitude, whichever end of
			// the range you take. A tighter number would need a distribution, and would not change the
			// decision it informs.
			t.Logf("RATIO: the enforcement hop (%v) is ~%.0fx smaller than time-to-first-token (%v, ONE sample; "+
				"cold calls have been observed 10x higher). The seam's deployment shape is not a latency "+
				"decision at this scale; pick it on operational grounds.",
				(remoteMedian - localMedian).Round(time.Microsecond),
				float64(ttfb)/float64(remoteMedian-localMedian),
				ttfb.Round(time.Millisecond))
		}
	} else {
		t.Log("AEON_TEST_MODELGW_ADDR not set — the streaming half of this measurement needs the real aeon-modelgw (see INT-008)")
	}
}

// measureFirstByte reports time-to-first-byte on the real streaming endpoint.
//
// First byte rather than total: a hop that adds to the total is a throughput cost, and one that adds before
// the first token is a cost a person can feel. They are different decisions and a single number conflates them.
func measureFirstByte(t *testing.T, addr string) time.Duration {
	t.Helper()
	// reasoning-local, not reasoning-high. The high profile routes to anthropic/openai, which this
	// deployment does not register, so it answers 502 "provider not registered" — and the first version of
	// this helper reported that as "no provider credentials configured", GUESSING a cause it had not
	// checked. The credentials were there; the profile's candidates were not. A diagnostic that invents a
	// reason sends the next reader to the wrong place, so the message below now prints what came back.
	body, _ := json.Marshal(map[string]any{
		"model":    "reasoning-local",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "say ok"}},
		// Short on purpose: this measures TIME TO FIRST BYTE, and a long generation would only add
		// tokens after the number being measured has already been taken.
		"max_tokens": 16,
	})
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building the streaming request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Logf("streaming measurement unavailable: %v", err)
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Logf("streaming measurement unavailable: the model gateway answered %d: %s", resp.StatusCode, bytes.TrimSpace(detail))
		return 0
	}
	buf := make([]byte, 1)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Logf("streaming measurement unavailable: reading the first byte: %v", err)
		return 0
	}
	ttfb := time.Since(start)
	t.Logf("%-38s time-to-first-byte=%v", "remote model gateway, streaming", ttfb.Round(time.Millisecond))
	return ttfb
}

// TestSeamShapesAreBothReachable keeps the measurement above honest about what it compared.
//
// Written because the failure mode of a benchmark that self-skips is reporting one shape and calling it a
// comparison. If the remote address is set but nothing answers on it, that is a broken environment and has
// to look different from an absent one.
func TestSeamShapesAreBothReachable(t *testing.T) {
	remote := os.Getenv("AEON_TEST_TOOLGW_ADDR")
	if remote == "" {
		t.Skip("AEON_TEST_TOOLGW_ADDR not set")
	}
	conn, err := net.DialTimeout("tcp", remote, 3*time.Second)
	if err != nil {
		t.Fatalf("AEON_TEST_TOOLGW_ADDR=%s is set but nothing is listening (%v) — the latency comparison would have measured one shape and called it two", remote, err)
	}
	conn.Close()
	t.Log(fmt.Sprintf("both shapes reachable: remote=%s and a local handler in-process", remote))
}

// seamCallerToken is the credential the latency measurement presents.
//
// The REMOTE shape is the real aeon-toolgw container, which verifies against the shipped development
// bundle — so this has to be a token from that bundle, not the one this package mints for its own
// wrapped servers. AEON_CALLER_TOKEN lets the target override it; the default is the bundle's
// `integration-test` caller, which is declared mayActAs deep-research-general@0.1.0.
func seamCallerToken() string {
	if t := os.Getenv("AEON_CALLER_TOKEN"); t != "" {
		return t
	}
	return "dev-test-token-not-a-secret"
}
