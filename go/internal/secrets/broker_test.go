package secrets

import (
	"strings"
	"testing"
	"time"
)

func TestBrokerIssueThenResolveReturnsTheRawValue(t *testing.T) {
	b := NewBroker(map[string]string{"demo": "sk-real-secret-value"})

	ref, expiresAt, err := b.Issue("demo", time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if ref == "" {
		t.Fatal("expected a non-empty lease reference")
	}
	if strings.Contains(ref, "sk-real-secret-value") {
		t.Fatalf("lease reference must never contain the raw secret, got %q", ref)
	}
	if !expiresAt.After(time.Now()) {
		t.Fatalf("expected expiresAt in the future, got %s", expiresAt)
	}

	value, err := b.Resolve(ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if value != "sk-real-secret-value" {
		t.Fatalf("Resolve returned %q, want the raw secret value", value)
	}
}

func TestBrokerIssueRejectsUnknownSecret(t *testing.T) {
	b := NewBroker(map[string]string{"demo": "value"})
	if _, _, err := b.Issue("does-not-exist", time.Minute); err == nil {
		t.Fatal("expected Issue to fail for an unknown secret name")
	}
}

func TestBrokerResolveRejectsUnknownLease(t *testing.T) {
	b := NewBroker(map[string]string{"demo": "value"})
	if _, err := b.Resolve("lease_does_not_exist"); err == nil {
		t.Fatal("expected Resolve to fail for an unknown lease reference")
	}
}

func TestBrokerResolveRejectsExpiredLease(t *testing.T) {
	b := NewBroker(map[string]string{"demo": "value"})
	ref, _, err := b.Issue("demo", time.Millisecond)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := b.Resolve(ref); err == nil {
		t.Fatal("expected Resolve to fail for an expired lease")
	}
}

func TestBrokerResolveRejectsRevokedLease(t *testing.T) {
	b := NewBroker(map[string]string{"demo": "value"})
	ref, _, err := b.Issue("demo", time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	b.Revoke(ref)
	if _, err := b.Resolve(ref); err == nil {
		t.Fatal("expected Resolve to fail for a revoked lease")
	}
}

func TestBrokerLeaseIsReusableUntilExpiry(t *testing.T) {
	b := NewBroker(map[string]string{"demo": "value"})
	ref, _, err := b.Issue("demo", time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := b.Resolve(ref); err != nil {
			t.Fatalf("Resolve call %d: %v", i, err)
		}
	}
}

func TestBrokerDefaultsTTLWhenNonPositive(t *testing.T) {
	b := NewBroker(map[string]string{"demo": "value"})
	_, expiresAt, err := b.Issue("demo", 0)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if expiresAt.Before(time.Now().Add(DefaultLeaseTTL - time.Second)) {
		t.Fatalf("expected the default TTL (%s) to apply, got expiry %s", DefaultLeaseTTL, expiresAt)
	}
}

func TestRevokeAllForOwnerRevokesOnlyThatOwnersLeases(t *testing.T) {
	b := NewBroker(map[string]string{"demo": "value"})
	refA, _, err := b.IssueForOwner("demo", time.Minute, "agent-a@1.0.0")
	if err != nil {
		t.Fatalf("IssueForOwner (a): %v", err)
	}
	refB, _, err := b.IssueForOwner("demo", time.Minute, "agent-b@1.0.0")
	if err != nil {
		t.Fatalf("IssueForOwner (b): %v", err)
	}
	refPlain, _, err := b.Issue("demo", time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	revoked := b.RevokeAllForOwner("agent-a@1.0.0")
	if revoked != 1 {
		t.Fatalf("RevokeAllForOwner returned %d, want 1", revoked)
	}
	if _, err := b.Resolve(refA); err == nil {
		t.Fatal("expected agent-a's lease to be revoked")
	}
	if _, err := b.Resolve(refB); err != nil {
		t.Fatalf("expected agent-b's lease to be unaffected, got: %v", err)
	}
	if _, err := b.Resolve(refPlain); err != nil {
		t.Fatalf("expected the owner-less lease to be unaffected, got: %v", err)
	}
}

func TestRevokeAllForOwnerWithEmptyOwnerRevokesNothing(t *testing.T) {
	b := NewBroker(map[string]string{"demo": "value"})
	ref, _, err := b.Issue("demo", time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if revoked := b.RevokeAllForOwner(""); revoked != 0 {
		t.Fatalf("RevokeAllForOwner(\"\") revoked %d leases, want 0", revoked)
	}
	if _, err := b.Resolve(ref); err != nil {
		t.Fatalf("expected the lease to still resolve: %v", err)
	}
}
