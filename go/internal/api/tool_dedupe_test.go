package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/aeon-ai/aeon/go/internal/policy"
	"github.com/aeon-ai/aeon/go/internal/toolexec"
)

// countingTool is the whole instrument of this test: TOOL-005 is not about what comes back, it is
// about how many times the effect happened. Asserting on the response would pass against a gateway
// that ran the tool twice and returned the second answer.
type countingTool struct {
	runs    atomic.Int64
	failFor int64 // fail while runs <= failFor, then succeed
}

func (c *countingTool) register(e *toolexec.Executor, name string) {
	e.Register(name, func(args map[string]any) (map[string]any, error) {
		n := c.runs.Add(1)
		if n <= c.failFor {
			return nil, fmt.Errorf("simulated transient failure on attempt %d", n)
		}
		return map[string]any{"status": "executed", "run": n, "args": args}, nil
	})
}

func newDedupeTestServer(t *testing.T, tool *countingTool, toolName string, withStore bool) *httptest.Server {
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
	tool.register(executor, toolName)

	handlers := &ToolGatewayHandlers{Policy: engine, Executor: executor}
	if withStore {
		handlers.Executions = newAPITestStore(t).ToolExecutions()
	}

	mux := http.NewServeMux()
	handlers.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postExecuteBody(t *testing.T, srv *httptest.Server, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/execute", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST /execute: %v", err)
	}
	defer resp.Body.Close()
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}

// TestToolExecutionIsDeduplicatedByIdempotencyKey is TOOL-005's acceptance test.
//
// The Tool Registry has required idempotency_key_fields from every tool with effects since
// TOOL-001, and until now nothing used them at execution time: the only real deduplication in the
// project was a file-backed ledger its own code labels "not for production use". This is the table
// that makes the crash/resume guarantee rest on something a second worker can also see.
func TestToolExecutionIsDeduplicatedByIdempotencyKey(t *testing.T) {
	// "search.web" is what examples/deep-research/policy_bundle.yaml permits, so the policy path is
	// the real one rather than a fixture written to agree with the test.
	const toolName = "search.web"
	const agentRef = "deep-research-general@0.1.0" // el principal del policy_bundle real lleva versión

	newKey := func(label string) string { return fmt.Sprintf("tool005-%s-%d", label, time.Now().UnixNano()) }

	t.Run("a repeated key returns the recorded result and does NOT execute again", func(t *testing.T) {
		tool := &countingTool{}
		srv := newDedupeTestServer(t, tool, toolName, true)
		key := newKey("repeat")
		body := map[string]any{
			"agent_manifest_ref": agentRef, "tool_name": toolName,
			"args": map[string]any{"query": "estado del arte"}, "idempotency_key": key,
		}

		status, first := postExecuteBody(t, srv, body)
		if status != http.StatusOK {
			t.Fatalf("first execute: status=%d body=%v", status, first)
		}
		if dup, _ := first["deduplicated"].(bool); dup {
			t.Fatal("the first call reported deduplicated")
		}

		for i := 0; i < 3; i++ {
			status, again := postExecuteBody(t, srv, body)
			if status != http.StatusOK {
				t.Fatalf("repeat %d: status=%d body=%v", i, status, again)
			}
			if dup, _ := again["deduplicated"].(bool); !dup {
				t.Fatalf("repeat %d did not report deduplicated: %v", i, again)
			}
			if fmt.Sprint(again["result"]) != fmt.Sprint(first["result"]) {
				t.Fatalf("repeat %d returned a different result: %v vs %v", i, again["result"], first["result"])
			}
		}

		if got := tool.runs.Load(); got != 1 {
			t.Fatalf("the tool ran %d times across 4 calls — deduplication means the EFFECT happens once, not that the answer matches", got)
		}
	})

	t.Run("the same key with different arguments is refused, not deduplicated", func(t *testing.T) {
		// The key is derived from the arguments, so this can only mean a derivation bug upstream —
		// and it is worse than a duplicate: deduplicating here would hand one call's result to a
		// different call.
		tool := &countingTool{}
		srv := newDedupeTestServer(t, tool, toolName, true)
		key := newKey("diverged")

		postExecuteBody(t, srv, map[string]any{
			"agent_manifest_ref": agentRef, "tool_name": toolName,
			"args": map[string]any{"query": "a"}, "idempotency_key": key,
		})
		status, body := postExecuteBody(t, srv, map[string]any{
			"agent_manifest_ref": agentRef, "tool_name": toolName,
			"args": map[string]any{"query": "b"}, "idempotency_key": key,
		})

		if status != http.StatusConflict {
			t.Fatalf("status = %d, want 409 for a key reused with different arguments (body=%v)", status, body)
		}
		if got := tool.runs.Load(); got != 1 {
			t.Fatalf("the tool ran %d times — the second call must not execute", got)
		}
	})

	t.Run("concurrent retries of the same key execute exactly once", func(t *testing.T) {
		// Two workers retrying one Temporal Activity at the same time is the case this exists for.
		// Whoever loses the race gets a typed, retryable rejection rather than a second execution.
		tool := &countingTool{}
		srv := newDedupeTestServer(t, tool, toolName, true)
		key := newKey("concurrent")
		body := map[string]any{
			"agent_manifest_ref": agentRef, "tool_name": toolName,
			"args": map[string]any{"query": "race"}, "idempotency_key": key,
		}

		const n = 8
		var wg sync.WaitGroup
		statuses := make([]int, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				statuses[i], _ = postExecuteBody(t, srv, body)
			}(i)
		}
		wg.Wait()

		if got := tool.runs.Load(); got != 1 {
			t.Fatalf("the tool ran %d times under %d concurrent calls, want exactly 1", got, n)
		}
		var ok, conflict int
		for _, s := range statuses {
			switch s {
			case http.StatusOK:
				ok++
			case http.StatusConflict:
				conflict++
			default:
				t.Errorf("unexpected status %d", s)
			}
		}
		if ok+conflict != n {
			t.Fatalf("statuses did not add up: %d ok, %d conflict, want %d total", ok, conflict, n)
		}
		t.Logf("%d concurrent calls -> %d executed, %d rejected as in-flight, tool ran %d time(s)", n, ok, conflict, tool.runs.Load())
	})

	t.Run("a second call while the first is genuinely in flight is rejected, not queued", func(t *testing.T) {
		// The concurrent subtest above proves the effect happens once, but it does NOT reach this
		// path: the first call finishes so fast that the others find the key already completed and
		// take the deduplicated branch. Without a tool that stays inside the executor, the in-flight
		// rejection, its 409 and its Retry-After header are untested code that looks covered.
		release := make(chan struct{})
		entered := make(chan struct{}, 1)
		executor := toolexec.NewExecutor()
		var runs atomic.Int64
		executor.Register(toolName, func(args map[string]any) (map[string]any, error) {
			runs.Add(1)
			entered <- struct{}{}
			<-release
			return map[string]any{"status": "executed"}, nil
		})

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
		mux := http.NewServeMux()
		(&ToolGatewayHandlers{Policy: engine, Executor: executor, Executions: newAPITestStore(t).ToolExecutions()}).Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)

		key := newKey("inflight")
		body := map[string]any{
			"agent_manifest_ref": agentRef, "tool_name": toolName,
			"args": map[string]any{"query": "slow"}, "idempotency_key": key,
		}

		done := make(chan int, 1)
		go func() {
			status, _ := postExecuteBody(t, srv, body)
			done <- status
		}()

		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the first call never reached the executor")
		}

		// The first call is now inside the tool, holding the claim.
		raw2, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+"/execute", "application/json", bytes.NewReader(raw2))
		if err != nil {
			t.Fatalf("second POST: %v", err)
		}
		secondStatus := resp.StatusCode
		retryAfter := resp.Header.Get("Retry-After")
		var secondBody map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&secondBody)
		resp.Body.Close()

		close(release)
		if first := <-done; first != http.StatusOK {
			t.Fatalf("the first call ended with status %d", first)
		}

		if secondStatus != http.StatusConflict {
			t.Fatalf("the in-flight call got status %d, want 409", secondStatus)
		}
		if retryAfter == "" {
			t.Error("no Retry-After on an in-flight rejection — a caller told to retry needs to know when")
		}
		if retryable, _ := secondBody["retryable"].(bool); !retryable {
			t.Errorf("the rejection did not declare itself retryable: %v", secondBody)
		}
		if got := runs.Load(); got != 1 {
			t.Fatalf("the tool ran %d times, want 1 — the second call must not enter the executor at all", got)
		}
	})

	t.Run("a failed execution releases the key so a retry can run, and the failure is counted", func(t *testing.T) {
		// A sticky failure would make one transient error permanent for that step forever. The
		// residual risk — an effect that landed before the error — is why the count survives instead
		// of the row being deleted.
		tool := &countingTool{failFor: 1}
		srv := newDedupeTestServer(t, tool, toolName, true)
		key := newKey("released")
		body := map[string]any{
			"agent_manifest_ref": agentRef, "tool_name": toolName,
			"args": map[string]any{"query": "transient"}, "idempotency_key": key,
		}

		if status, _ := postExecuteBody(t, srv, body); status == http.StatusOK {
			t.Fatal("the first call was supposed to fail")
		}
		status, second := postExecuteBody(t, srv, body)
		if status != http.StatusOK {
			t.Fatalf("the retry after a failure got status=%d body=%v — a transient error must not lock the step out forever", status, second)
		}
		if attempts, _ := second["failed_attempts"].(float64); attempts != 1 {
			t.Errorf("failed_attempts = %v, want 1 — a previous attempt that may have landed an effect must stay visible", second["failed_attempts"])
		}
		if got := tool.runs.Load(); got != 2 {
			t.Fatalf("the tool ran %d times, want 2 (one failure, one retry)", got)
		}
	})

	t.Run("asking for deduplication without a store configured refuses to execute", func(t *testing.T) {
		// The dangerous shape would be to run it anyway: the caller asked for protection and would
		// get an effect instead, silently.
		tool := &countingTool{}
		srv := newDedupeTestServer(t, tool, toolName, false)

		status, _ := postExecuteBody(t, srv, map[string]any{
			"agent_manifest_ref": agentRef, "tool_name": toolName,
			"args": map[string]any{"query": "x"}, "idempotency_key": newKey("nostore"),
		})
		if status != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", status)
		}
		if got := tool.runs.Load(); got != 0 {
			t.Fatalf("the tool ran %d times despite the gateway being unable to deduplicate", got)
		}
	})

	t.Run("policy still runs first: a denied tool is never recorded nor executed", func(t *testing.T) {
		// Deduplication must never precede the policy check, or a replayed result could serve a call
		// that policy would deny today.
		tool := &countingTool{}
		srv := newDedupeTestServer(t, tool, "shell.exec", true)
		key := newKey("denied")

		status, _ := postExecuteBody(t, srv, map[string]any{
			"agent_manifest_ref": agentRef, "tool_name": "shell.exec",
			"args": map[string]any{"command": "echo hi"}, "idempotency_key": key,
		})
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
		if got := tool.runs.Load(); got != 0 {
			t.Fatalf("a forbidden tool ran %d times", got)
		}

		// And nothing was claimed, so the key is still free.
		s := newAPITestStore(t)
		claim, err := s.ToolExecutions().Claim(context.Background(), key, "shell.exec", agentRef, map[string]any{"command": "echo hi"})
		if err != nil {
			t.Fatalf("claiming the key afterwards: %v", err)
		}
		if !claim.Claimed {
			t.Error("the denied call left a claim behind — a policy denial is not an execution")
		}
	})
}
