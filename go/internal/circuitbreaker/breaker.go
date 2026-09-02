// Package circuitbreaker decides, from a rolling window of recent run outcomes per agent version,
// whether that version should trip into quarantine (A5) — an automatic circuit breaker against a
// failure-rate or cost anomaly. This package is pure decision logic, deliberately with no Postgres
// or HTTP dependency of its own: it holds an in-memory window and returns a typed Verdict; the
// caller (go/internal/api/circuit_breaker_handlers.go) applies a tripped Verdict to the real,
// durable state in store.AgentRegistry.Quarantine — the same separation ADR-001 already establishes
// between a Decision and whoever applies it.
//
// Deliberately bounded scope (see backlog.md): nothing yet calls RecordOutcome automatically when a
// real run finishes — the Python worker has no hook that reports a run's outcome back to the
// control plane today. This package is the real, tested breaker engine and enforcement primitive;
// wiring a real run's completion to actually call it is separate integration work, the same shape
// of gap MEM-003/EVAL-004 documented for Reflection/Learning Eval before their own workflow wiring
// existed.
package circuitbreaker

import (
	"fmt"
	"sync"
)

// Thresholds configures when a rolling window trips.
type Thresholds struct {
	// WindowSize bounds how many of the most recent outcomes are kept per agent version.
	WindowSize int
	// MinSamples is the fewest observations required before a trip can fire at all — avoids a
	// single early failure quarantining a version that has barely run yet.
	MinSamples int
	// MaxFailureRate: trip if the window's failure fraction exceeds this (0.0-1.0).
	MaxFailureRate float64
	// MaxAvgCostUSD: trip if the window's average cost per run exceeds this. Zero disables the cost
	// check entirely (a version with no cost anomaly detection configured).
	MaxAvgCostUSD float64
}

// DefaultThresholds is a reasonable starting point: 10-run window, at least 5 samples before a
// trip can fire, more than half failing trips it, no cost-anomaly check (MaxAvgCostUSD 0 disables
// it) since "anomalous" cost is workload-specific and has no safe platform-wide default.
var DefaultThresholds = Thresholds{WindowSize: 10, MinSamples: 5, MaxFailureRate: 0.5, MaxAvgCostUSD: 0}

// Observation is one real run's outcome, as reported by whatever eventually calls RecordOutcome.
type Observation struct {
	Success bool
	CostUSD float64
}

// Verdict is RecordOutcome's result: whether this observation tripped the breaker, and — only when
// Tripped — a human-readable reason suitable for AgentRegistry.Quarantine's reason column.
type Verdict struct {
	Tripped bool
	Reason  string
}

type agentKey struct {
	name    string
	version string
}

// Breaker holds one rolling window of Observations per agent version. Not persisted — a process
// restart starts every version with a clean window; the durable, cross-process state is whatever
// AgentRegistry.Quarantine actually recorded, which the caller is responsible for applying.
type Breaker struct {
	mu         sync.Mutex
	thresholds Thresholds
	windows    map[agentKey][]Observation
	quarantine map[agentKey]bool // this process's own view — see IsQuarantined's doc
}

// New builds a Breaker. A zero-value Thresholds is replaced with DefaultThresholds.
func New(thresholds Thresholds) *Breaker {
	if thresholds.WindowSize <= 0 {
		thresholds = DefaultThresholds
	}
	return &Breaker{
		thresholds: thresholds,
		windows:    map[agentKey][]Observation{},
		quarantine: map[agentKey]bool{},
	}
}

// RecordOutcome appends obs to name@version's rolling window (bounded to WindowSize, oldest
// dropped first) and evaluates whether it should trip. Once a version has tripped, subsequent calls
// keep reporting Tripped=true until Reset is called (mirroring the fact that a quarantined version
// stays quarantined until an operator or later remediation clears it) — this avoids the breaker
// "un-tripping itself" the moment enough fresh, unrelated successes push old failures out of the
// window while the underlying agent is still quarantined in the registry.
func (b *Breaker) RecordOutcome(name, version string, obs Observation) Verdict {
	b.mu.Lock()
	defer b.mu.Unlock()

	key := agentKey{name, version}
	if b.quarantine[key] {
		return Verdict{Tripped: true, Reason: "already quarantined"}
	}

	window := append(b.windows[key], obs)
	if len(window) > b.thresholds.WindowSize {
		window = window[len(window)-b.thresholds.WindowSize:]
	}
	b.windows[key] = window

	if len(window) < b.thresholds.MinSamples {
		return Verdict{}
	}

	var failures int
	var totalCost float64
	for _, o := range window {
		if !o.Success {
			failures++
		}
		totalCost += o.CostUSD
	}
	failureRate := float64(failures) / float64(len(window))
	avgCost := totalCost / float64(len(window))

	if failureRate > b.thresholds.MaxFailureRate {
		b.quarantine[key] = true
		return Verdict{Tripped: true, Reason: fmt.Sprintf(
			"failure rate %.2f over the last %d run(s) exceeds threshold %.2f", failureRate, len(window), b.thresholds.MaxFailureRate,
		)}
	}
	if b.thresholds.MaxAvgCostUSD > 0 && avgCost > b.thresholds.MaxAvgCostUSD {
		b.quarantine[key] = true
		return Verdict{Tripped: true, Reason: fmt.Sprintf(
			"average cost $%.4f over the last %d run(s) exceeds threshold $%.4f", avgCost, len(window), b.thresholds.MaxAvgCostUSD,
		)}
	}
	return Verdict{}
}

// IsQuarantined reports this process's own view of whether name@version has tripped — a
// convenience for callers already holding a Breaker; it is NOT the authoritative, cross-process
// answer (that's store.AgentRegistry.Get(...).Quarantined, the durable state every process should
// actually enforce against).
func (b *Breaker) IsQuarantined(name, version string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.quarantine[agentKey{name, version}]
}

// Reset clears name@version's window and this process's quarantine flag — pairs with
// AgentRegistry.Unquarantine, the manual reset after remediation.
func (b *Breaker) Reset(name, version string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := agentKey{name, version}
	delete(b.windows, key)
	delete(b.quarantine, key)
}
