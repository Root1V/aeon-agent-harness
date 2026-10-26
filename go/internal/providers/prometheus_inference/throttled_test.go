package prometheusinference_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aeon-ai/aeon/go/internal/providers"
	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
)

// MDL-024: a rate limit must arrive as a WAIT and not as a failure.
//
// THE DEFECT, found reading VRT-PRM-007's closing note: the SDK hands us a fully typed refusal —
// status, the problem-details type, a resolved Retry-After and the window's counters — and this
// adapter wrapped it with `fmt.Errorf("%w")`. The gateway, which cannot import anyone's SDK, then saw
// only a string, counted the candidate as failed, and both surfaces answered 502 «all candidates
// failed». A transitory condition arrived as a permanent failure, with no wait and nothing to tell it
// apart from a provider that is down — so the caller retried immediately with the same request.
//
// Driven through the REAL SDK against an upstream that really answers 429 with problem+json, because
// the whole value of this change is that the platform's own classification survives: a fake provider
// returning a hand-made error would be asserting that our code handles an error we wrote.
func TestARateLimitArrivesAsAWaitAndNotAsAFailure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/oauth2/token") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "test-token", "token_type": "Bearer", "expires_in": 600,
				"scope": "inference:read model:test-model",
			})
			return
		}
		calls.Add(1)
		// The platform's real shape for this: RFC 9457 problem details, the window's own headers, and
		// Retry-After. Zero seconds so the SDK's three attempts cost no wall clock — the attempts
		// themselves are the thing being exercised, not the sleeping.
		w.Header().Set("Retry-After", "0")
		w.Header().Set("X-RateLimit-Scope", "chat_completions")
		w.Header().Set("X-RateLimit-Limit-Requests", "60")
		w.Header().Set("X-RateLimit-Remaining-Requests", "0")
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":   "https://api.example.com/problems/rate-limit-exceeded-requests",
			"title":  "Too Many Requests",
			"detail": "client has exceeded its request limit",
		})
	}))
	t.Cleanup(srv.Close)

	adapter := prometheusinference.New("", srv.URL, "id", "secret", "inference:read model:test-model", "test-model")
	_, err := adapter.Decide(context.Background(),
		map[string]any{"model": "test-model", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if err == nil {
		t.Fatal("a rate-limited call returned no error")
	}

	throttle, ok := providers.ThrottleOf(err)
	if !ok {
		t.Fatalf("the error is %T (%v), want a *providers.Throttled — without the type the gateway sees "+
			"a string and answers 502, which is a permanent failure for a transitory condition", err, err)
	}
	if throttle.Provider != "prometheus_inference" || throttle.Model != "test-model" {
		t.Errorf("throttle names %s/%s, want prometheus_inference/test-model: a cascade has to report "+
			"WHICH candidate is limited", throttle.Provider, throttle.Model)
	}
	if throttle.RetryAfterSeconds == nil {
		t.Error("no retry_after survived, which is the one fact the caller cannot derive and the whole " +
			"reason this is not just a nicer error message")
	}
	if throttle.Scope != "chat_completions" {
		t.Errorf("scope = %q, want chat_completions — the endpoints hold separate windows, so a wait "+
			"learned from one says nothing about another", throttle.Scope)
	}
	// The SDK's own policy already slept and retried before giving up: reaching our code means it is
	// out of attempts, which is exactly when the caller should be told to wait rather than to retry.
	if got := calls.Load(); got != 3 {
		t.Errorf("the upstream saw %d attempts, want the SDK's 3 — if it is 1 the SDK is not retrying, "+
			"and our answer would be telling the caller to wait on something nobody waited on", got)
	}
}

// TestAnythingElseIsStillAnOrdinaryFailure is the other half: only the rate limit is translated.
//
// The SDK marks several other errors retryable (`backend-unavailable`, `upstream-error`) and those are
// 5xx that already produce a 502 the caller's worker retries — a status that is already right. What was
// wrong was the one case where the platform told us HOW LONG and we dropped it. Translating more would
// turn a 502 a worker already handles into a 429 promising a wait nobody supplied.
func TestAnythingElseIsStillAnOrdinaryFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/oauth2/token") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "t", "token_type": "Bearer", "expires_in": 600,
				"scope": "inference:read model:test-model",
			})
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":  "https://api.example.com/problems/backend-unavailable",
			"title": "Service Unavailable",
		})
	}))
	t.Cleanup(srv.Close)

	adapter := prometheusinference.New("", srv.URL, "id", "secret", "inference:read model:test-model", "test-model")
	_, err := adapter.Decide(context.Background(),
		map[string]any{"model": "test-model", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if err == nil {
		t.Fatal("an unavailable backend returned no error")
	}
	if _, ok := providers.ThrottleOf(err); ok {
		t.Errorf("a backend-unavailable was translated into a throttle: %v — a 429 promising a wait "+
			"nobody supplied is worse than the 502 a worker already retries", err)
	}
}
