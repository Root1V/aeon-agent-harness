// Package secrets implements a minimal Secret Broker (SEC-002): callers get short-lived, opaque
// lease references instead of raw credential values. A raw secret is resolved exactly once it's
// actually needed — inside a tool's own server-side execution (see
// go/internal/toolexec/executor.go's "secrets.whoami" registration) — never handed back to
// whatever requested the lease, and never something a caller could put into a tool result, a log
// line, or anything that could reach a rendered model context.
//
// Scope, real and deliberately bounded: this replaces the MVP's static, long-lived env-var secrets
// (go/cmd/aeon-modelgw/main.go's provider API keys, still read directly from the environment) with
// a real lease/expiry model for *new* secret-consuming tools — it does not retrofit the existing
// provider adapters, which have their own credential lifetimes already (see
// go/internal/providers/prometheus_inference/auth.go's own OAuth2 client_credentials TokenSource,
// a separate, pre-existing example of short-lived credentials this package doesn't replace).
// Workload identity (SPIFFE/SVID, named in the architecture doc alongside this feature) is not
// implemented — that needs a SPIRE server and workload attestation infrastructure, a real
// prerequisite of its own scope, tracked in backlog.md rather than attempted here.
package secrets

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// DefaultLeaseTTL is used when Issue is called with ttl<=0.
const DefaultLeaseTTL = 5 * time.Minute

type lease struct {
	secretName string
	expiresAt  time.Time
	owner      string // e.g. "agent-name@1.0.0" — empty for a lease issued with no owner (Issue)
}

// Broker holds named secret values (never exposed directly except via Resolve) and issues
// short-lived lease references against them.
type Broker struct {
	mu      sync.Mutex
	secrets map[string]string
	leases  map[string]lease
}

// NewBroker builds a Broker holding the given named secret values directly. Real deployments would
// source these from wherever they actually live (today: environment variables, see
// NewBrokerFromEnv; later: Vault, AWS Secrets Manager, ...) — this constructor doesn't care where
// they came from, which is what makes it easy for tests to inject known values.
func NewBroker(values map[string]string) *Broker {
	copied := make(map[string]string, len(values))
	for k, v := range values {
		copied[k] = v
	}
	return &Broker{secrets: copied, leases: map[string]lease{}}
}

// NewBrokerFromEnv builds a Broker whose secret values come from AEON_SECRET_<NAME> environment
// variables — the same "config-as-code from environment" convention every provider credential in
// this project already follows (go/cmd/aeon-modelgw/main.go). A name with no corresponding
// non-empty env var is simply absent (Issue for it fails with a clear error) rather than the
// process refusing to start.
func NewBrokerFromEnv(names []string) *Broker {
	values := make(map[string]string, len(names))
	for _, name := range names {
		if v := os.Getenv(envVarName(name)); v != "" {
			values[name] = v
		}
	}
	return NewBroker(values)
}

func envVarName(name string) string {
	return "AEON_SECRET_" + strings.ToUpper(strings.ReplaceAll(name, ".", "_"))
}

// Issue mints a short-lived, opaque lease reference for the named secret — never the secret value
// itself. The lease is valid (repeatedly resolvable, like Vault's dynamic-secret leases, not
// single-use) until it expires or is explicitly Revoked. Equivalent to IssueForOwner with an empty
// owner (a lease no circuit breaker/kill switch (A5) can revoke by owner).
func (b *Broker) Issue(name string, ttl time.Duration) (ref string, expiresAt time.Time, err error) {
	return b.IssueForOwner(name, ttl, "")
}

// IssueForOwner is Issue, additionally tagging the lease with owner (e.g. an agent identity,
// "name@version") so a later RevokeAllForOwner can cut off every credential a specific agent
// version currently holds — the "revocación de credenciales en caliente" half of A5's circuit
// breaker + kill switch, without needing to know individual lease refs.
func (b *Broker) IssueForOwner(name string, ttl time.Duration, owner string) (ref string, expiresAt time.Time, err error) {
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.secrets[name]; !ok {
		return "", time.Time{}, fmt.Errorf("secrets: unknown secret %q", name)
	}

	ref, err = randomRef()
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt = time.Now().Add(ttl)
	b.leases[ref] = lease{secretName: name, expiresAt: expiresAt, owner: owner}
	return ref, expiresAt, nil
}

// Resolve returns the raw secret value for a still-valid lease reference. This is the only function
// in this package that returns a raw secret value — callers MUST use it only at the point of actual
// external use (e.g. setting an Authorization header on an outbound request), never to pass the
// value onward into a tool result, a log line, or anything that could reach a rendered model
// context.
func (b *Broker) Resolve(ref string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	l, ok := b.leases[ref]
	if !ok {
		return "", fmt.Errorf("secrets: unknown or already-revoked lease")
	}
	if time.Now().After(l.expiresAt) {
		delete(b.leases, ref)
		return "", fmt.Errorf("secrets: lease expired at %s", l.expiresAt.Format(time.RFC3339))
	}
	value, ok := b.secrets[l.secretName]
	if !ok {
		return "", fmt.Errorf("secrets: secret %q no longer available", l.secretName)
	}
	return value, nil
}

// Revoke immediately invalidates a lease, before its natural expiry.
func (b *Broker) Revoke(ref string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.leases, ref)
}

// RevokeAllForOwner immediately invalidates every still-active lease tagged with owner (see
// IssueForOwner) — a real "revoke this agent's credentials right now" kill switch, without the
// caller needing to track individual lease refs. Returns how many leases were revoked. A lease
// issued via the plain Issue (empty owner) is never matched by a non-empty owner query.
func (b *Broker) RevokeAllForOwner(owner string) int {
	if owner == "" {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	revoked := 0
	for ref, l := range b.leases {
		if l.owner == owner {
			delete(b.leases, ref)
			revoked++
		}
	}
	return revoked
}

func randomRef() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("secrets: generating lease reference: %w", err)
	}
	return "lease_" + hex.EncodeToString(buf), nil
}
