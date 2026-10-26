package providers

import (
	"errors"
	"fmt"
)

// Throttled is "the provider refused FOR NOW", in a form the routing core can read without knowing
// whose SDK produced it (MDL-024).
//
// WHY A TYPE AND NOT A STRING, which is the whole defect this closes: the platform hands our adapter
// a fully typed refusal — status, the problem-details type, a Retry-After, and the window's own
// counters — and the adapter used to wrap it with `fmt.Errorf("%w")`. From there the gateway counted
// the candidate as failed and both surfaces answered 502 «all candidates failed». So a TRANSITORY
// condition arrived as a PERMANENT failure, with no wait and nothing to tell it apart from a provider
// that is down, and the caller retried immediately with the same request.
//
// It is the same shape as MDL-023 one level up: a provider outcome flattened into "everything failed".
// Found reading VRT-PRM-007, where Veritium measured the token-per-minute limit against Prometheus —
// their half is Prometheus's, and this half is ours and survives even if Prometheus fixes theirs.
//
// PROVIDER-AGNOSTIC ON PURPOSE. The adapter that owns the platform's SDK translates into this; the
// routing core never imports anyone's SDK, which is ADR-004's rule and the reason this package exists.
type Throttled struct {
	// Provider and Model are who refused, so a cascade can report WHICH candidate is throttled rather
	// than that "something" is.
	Provider string
	Model    string
	// Scope is the budget the platform says was exhausted ("chat_completions", "embeddings", …), empty
	// when it does not say. Reported rather than assumed: the endpoints hold separate windows, so a
	// wait learned from one says nothing about another.
	Scope string
	// RetryAfterSeconds is how long the platform said to wait, and it is a POINTER because "it did not
	// say" is a third state. A zero would read as "retry now", which is the one thing a throttled
	// caller must not do.
	RetryAfterSeconds *float64
	// Err is the platform's own error, kept for errors.Is/As by whoever does own that SDK.
	Err error
}

func (t *Throttled) Error() string {
	wait := "no wait supplied"
	if t.RetryAfterSeconds != nil {
		wait = fmt.Sprintf("retry after %.0fs", *t.RetryAfterSeconds)
	}
	scope := t.Scope
	if scope == "" {
		scope = "unspecified scope"
	}
	return fmt.Sprintf("providers: %s/%s is rate limited (%s, %s)", t.Provider, t.Model, scope, wait)
}

func (t *Throttled) Unwrap() error { return t.Err }

// ErrThrottled is the sentinel, so a caller can ask the question without naming the type.
var ErrThrottled = errors.New("providers: provider is rate limited")

// Is makes errors.Is(err, ErrThrottled) true for any Throttled.
func (t *Throttled) Is(target error) bool { return target == ErrThrottled }

// ThrottleOf returns the Throttled in err's chain, if any.
func ThrottleOf(err error) (*Throttled, bool) {
	var t *Throttled
	if errors.As(err, &t) {
		return t, true
	}
	return nil, false
}

// SoonestRetry returns the shortest wait among throttles, and whether every error was one.
//
// EVERY ONE, and that is the condition the surfaces check before answering 429: one candidate down for
// another reason means the call failed for a reason a wait will not fix, and answering 429 there would
// tell the caller to retry into a cascade that is still broken. The MINIMUM is the right wait for the
// same reason: it is the soonest moment at which anything in the pool could work.
func SoonestRetry(errs []error) (wait *float64, allThrottled bool) {
	if len(errs) == 0 {
		return nil, false
	}
	allThrottled = true
	for _, err := range errs {
		t, ok := ThrottleOf(err)
		if !ok {
			allThrottled = false
			continue
		}
		if t.RetryAfterSeconds == nil {
			continue
		}
		if wait == nil || *t.RetryAfterSeconds < *wait {
			seconds := *t.RetryAfterSeconds
			wait = &seconds
		}
	}
	return wait, allThrottled
}
