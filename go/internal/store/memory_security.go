package store

import (
	"context"
	"fmt"
)

// RepairIfTampered is SEC-004's automated response to a poisoning attempt: it recomputes
// memoryID's hash/provenance_hmac from its own current content/refs and this store's key
// (VerifyProvenance) and, if they no longer match — meaning the content or a provenance field was
// altered by something other than this MemoryStore's own writes — revokes it immediately,
// regardless of its current status. There is no attempt to reconstruct or trust the original
// content: "repair" here means removing the untrustworthy record from use, the same
// never-silently-trust-tampered-input discipline as DR-005's citation repair (which repairs a
// citation FROM the ledger, never invents one) applied to memory integrity instead of citations.
// Returns tampered=true when a repair (revocation) happened, false when the record checked out
// and was left untouched.
func (s *MemoryStore) RepairIfTampered(ctx context.Context, memoryID string) (tampered bool, rec *MemoryRecord, err error) {
	current, err := s.Get(ctx, memoryID)
	if err != nil {
		return false, nil, err
	}
	if s.VerifyProvenance(current) {
		return false, current, nil
	}
	repaired, err := s.emergencyRevoke(ctx, memoryID)
	if err != nil {
		return false, nil, err
	}
	return true, repaired, nil
}

// emergencyRevoke moves memoryID straight to REVOKED regardless of its current status — unlike
// memoryValidTransitions/memoryPostActiveTransitions (which enforce the governed pipeline's
// forward-only progression), a confirmed poisoning finding must be actionable from ANY status,
// the same way a real incident response can't wait for a record to reach the "correct" stage of
// its own lifecycle before being pulled. A record already REVOKED is left as-is (no-op, not an
// error) since it's already achieved the intended end state.
func (s *MemoryStore) emergencyRevoke(ctx context.Context, memoryID string) (*MemoryRecord, error) {
	_, err := s.pool.Exec(ctx,
		`UPDATE memory_records SET status = $1, updated_at = now() WHERE memory_id = $2 AND status != $1`,
		MemoryStatusRevoked, memoryID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: emergency revoke: %w", err)
	}
	return s.Get(ctx, memoryID)
}
