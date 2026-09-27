package prometheusinference_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	prometheusinference "github.com/aeon-ai/aeon/go/internal/providers/prometheus_inference"
)

// TestTokenSourceSurvivesLocalClockJump is MDL-010's acceptance test, and it changed shape twice —
// both times because going to the code contradicted the premise.
//
// FIRST correction, already recorded in the roadmap: the item was born as "anchor expiry to the
// response Date header", from a concession by Axonium. Reading our own code showed that would REPLACE a
// correct mechanism with a worse one.
//
// SECOND correction, this one: the code the item describes no longer exists. MDL-009 deleted our
// TokenSource with auth.go, and the Axonium SDK owns expiry now. So the property still matters and the
// implementation is someone else's — which changes what this test can honestly assert.
//
// What their implementation does, read from the module cache rather than assumed:
//
//	serverRemaining := time.Duration(exp-issued.Unix()) * time.Second   // BOTH readings server-side
//	expiresAt := time.Now().Add(lifetime)                               // local MONOTONIC countdown
//
// That is better than the item proposed. The lifetime comes from the difference between two server-side
// values — the JWT's exp and the response's Date — so no skew between our clock and theirs can touch it.
// The countdown then uses time.Now, which carries a monotonic reading that Add preserves and Until
// reads, so a wall-clock jump or an NTP correction cannot shorten or extend it either.
//
// That design is testable from outside WITHOUT touching the system clock, which is the whole reason
// this test can exist: shift both server-side readings together and the remaining lifetime must not
// move, because only their difference is used.
//
// What this test does NOT cover, said plainly: a real suspend/resume, where the monotonic clock does
// not advance and a token can come back already expired on the server's reckoning. The residue there is
// the 401 -> invalidate -> re-mint path, which is the SDK's and is covered by ITS suite. Offering them a
// clock-jump case is in the channel.
func TestTokenSourceSurvivesLocalClockJump(t *testing.T) {
	ctx := context.Background()

	// jwtWithExp builds a token whose claims carry exp. Unsigned on purpose: the SDK reads claims to
	// learn the server's own expiry and does not verify a signature it has no key for.
	jwtWithExp := func(exp int64) string {
		enc := func(v any) string {
			raw, _ := json.Marshal(v)
			return base64.RawURLEncoding.EncodeToString(raw)
		}
		return enc(map[string]any{"alg": "none", "typ": "JWT"}) + "." +
			enc(map[string]any{"exp": exp, "sub": "test"}) + ".signature"
	}

	// authGateway serves tokens and one chat response, counting how many tokens it issued.
	newGateway := func(t *testing.T, serverNow time.Time, expiresIn int, mints *int64) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The Date header and the JWT exp are both the SERVER's view, and they move together here.
			w.Header().Set("Date", serverNow.UTC().Format(http.TimeFormat))
			if r.URL.Path == "/oauth2/token" || len(r.URL.Path) > 13 && r.URL.Path[len(r.URL.Path)-13:] == "/oauth2/token" {
				atomic.AddInt64(mints, 1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": jwtWithExp(serverNow.Add(time.Duration(expiresIn) * time.Second).Unix()),
					"token_type":   "Bearer",
					"expires_in":   expiresIn,
					"scope":        "inference:read model:m",
				})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model": "m",
				"choices": []any{map[string]any{
					"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": "ok"},
				}},
				"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
			})
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	call := func(t *testing.T, client *prometheusinference.Client) {
		t.Helper()
		if _, err := client.ChatCompletion(ctx, map[string]any{
			"model":    "m",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}); err != nil {
			t.Fatalf("chat completion: %v", err)
		}
	}

	t.Run("a long-lived token is minted once and reused", func(t *testing.T) {
		// The baseline. If this failed, every assertion about WHEN a token is re-minted would be
		// meaningless, because it would be re-minted always.
		var mints int64
		srv := newGateway(t, time.Now(), 3600, &mints)
		client := &prometheusinference.Client{GatewayURL: srv.URL, ClientID: "id", ClientSecret: "s", Scope: "inference:read model:m"}
		t.Cleanup(func() { _ = client.Close() })

		for range 4 {
			call(t, client)
		}
		if got := atomic.LoadInt64(&mints); got != 1 {
			t.Fatalf("minted %d tokens for 4 calls, want 1 — expiry is not being measured, it is being ignored", got)
		}
	})

	t.Run("a server clock an hour off changes nothing, because only the DIFFERENCE is used", func(t *testing.T) {
		// This is the property, and the reason it is testable without touching our clock: the Date header
		// and the JWT exp both move forward an hour together, so the remaining lifetime is identical. A
		// client that compared the server's Date against its OWN clock would see an hour of skew here and
		// either expire the token immediately or hold it an hour too long.
		for _, skew := range []time.Duration{-time.Hour, 0, time.Hour} {
			t.Run(fmt.Sprintf("skew=%s", skew), func(t *testing.T) {
				var mints int64
				srv := newGateway(t, time.Now().Add(skew), 3600, &mints)
				client := &prometheusinference.Client{GatewayURL: srv.URL, ClientID: "id", ClientSecret: "s", Scope: "inference:read model:m"}
				t.Cleanup(func() { _ = client.Close() })

				for range 3 {
					call(t, client)
				}
				if got := atomic.LoadInt64(&mints); got != 1 {
					t.Errorf("with the server clock %s off, minted %d tokens for 3 calls, want 1", skew, got)
				}
			})
		}
	})

	t.Run("a token the server says is already expired is not held", func(t *testing.T) {
		// The other end of the same rule. exp is BEHIND the server's Date, so by the server's own
		// reckoning there is nothing left — and the SDK re-mints rather than spending a request to
		// discover a 401. Asserting "more than one mint" rather than an exact count on purpose: how many
		// times it retries is its policy, not our property.
		var mints int64
		srv := newGateway(t, time.Now(), -60, &mints)
		client := &prometheusinference.Client{GatewayURL: srv.URL, ClientID: "id", ClientSecret: "s", Scope: "inference:read model:m"}
		t.Cleanup(func() { _ = client.Close() })

		for range 3 {
			_, _ = client.ChatCompletion(ctx, map[string]any{
				"model":    "m",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			})
		}
		if got := atomic.LoadInt64(&mints); got < 2 {
			t.Errorf("minted %d token(s) for 3 calls against an already-expired token, want more than 1 — a dead token was being reused", got)
		}
	})
}
