package store

import (
	"context"
	"fmt"
	"math"
	"time"
)

// memoryPostActiveTransitions is MEM-005's own state machine — separate from
// memoryValidTransitions (MEM-002) so the pre-ACTIVE candidate pipeline and post-ACTIVE
// utility/forgetting stay independently reasoned about. In particular, MEM-002's Reject
// deliberately still cannot touch an ACTIVE record (see memory_pipeline_test.go) — only Supersede
// and Revoke below can, and only for the reasons MEM-005 governs (a newer version replacing this
// one, low usefulness, or explicit revocation).
var memoryPostActiveTransitions = map[string]map[string]bool{
	MemoryStatusActive: {MemoryStatusSuperseded: true, MemoryStatusRevoked: true},
}

const (
	// usageSuccessDelta/usageFailureDelta are RecordUsage's fixed utility_score adjustments — a
	// simple, real signal (not a placeholder): every time a memory is actually used, its recorded
	// outcome moves the score, rather than utility_score sitting untouched from creation forever.
	usageSuccessDelta = 1.0
	usageFailureDelta = -0.5
)

// RecordUsage adjusts an ACTIVE memory's utility_score based on real usage feedback (a run that
// consulted this memory reports back whether it helped) and stamps last_used_at — the timestamp
// Prune's decay is measured from. utility_score never goes below 0 (mirrors the schema's own
// minimum:0 constraint).
func (s *MemoryStore) RecordUsage(ctx context.Context, memoryID string, success bool) (*MemoryRecord, error) {
	current, err := s.Get(ctx, memoryID)
	if err != nil {
		return nil, err
	}
	if current.Status != MemoryStatusActive {
		return nil, fmt.Errorf("%w: usage can only be recorded against an ACTIVE memory, %s is %s", ErrInvalidMemoryTransition, memoryID, current.Status)
	}

	delta := usageFailureDelta
	if success {
		delta = usageSuccessDelta
	}
	newScore := current.UtilityScore + delta
	if newScore < 0 {
		newScore = 0
	}

	_, err = s.pool.Exec(ctx,
		`UPDATE memory_records SET utility_score = $1, last_used_at = now(), updated_at = now() WHERE memory_id = $2`,
		newScore, memoryID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: record memory usage: %w", err)
	}
	return s.Get(ctx, memoryID)
}

// DecayedUtility is the pure exponential-decay function Prune uses: utilityScore halves every
// halfLifeDays that pass with no recorded use. A non-positive halfLifeDays or a since that isn't
// before asOf returns utilityScore unchanged (no decay to apply yet).
func DecayedUtility(utilityScore float64, since, asOf time.Time, halfLifeDays float64) float64 {
	if halfLifeDays <= 0 {
		return utilityScore
	}
	days := asOf.Sub(since).Hours() / 24
	if days <= 0 {
		return utilityScore
	}
	return utilityScore * math.Pow(0.5, days/halfLifeDays)
}

// Supersede retires memoryID (ACTIVE -> SUPERSEDED) because supersededByID — typically a newer
// version's memory_id, once it has itself been promoted — is meant to replace it. This store
// doesn't compare the two records' content; that judgement belongs to whoever calls Supersede
// (e.g. Reflection producing an updated candidate for the same fact).
func (s *MemoryStore) Supersede(ctx context.Context, memoryID, supersededByID string) (*MemoryRecord, error) {
	if _, err := s.transitionStatus(ctx, memoryID, MemoryStatusSuperseded, memoryPostActiveTransitions); err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE memory_records SET superseded_by = $1 WHERE memory_id = $2`, supersededByID, memoryID); err != nil {
		return nil, fmt.Errorf("store: set superseded_by: %w", err)
	}
	return s.Get(ctx, memoryID)
}

// Revoke retires an ACTIVE memory directly to REVOKED — explicit invalidation (e.g. a SEC-004
// poisoning finding, or an operator decision), distinct from MEM-002's Reject which only ever
// applies to a pre-ACTIVE candidate still in the pipeline.
func (s *MemoryStore) Revoke(ctx context.Context, memoryID string) (*MemoryRecord, error) {
	return s.transitionStatus(ctx, memoryID, MemoryStatusRevoked, memoryPostActiveTransitions)
}

// Prune finds every ACTIVE record in scopes+tenantID whose utility_score — decayed from
// last_used_at (or created_at, if it has never been used) to asOf, with the given half-life —
// falls below threshold, and revokes it: "forgetting" a memory nobody has found useful in a long
// time. Returns the memory_ids it pruned, so a caller can log/audit what was forgotten (no silent
// pruning — see the roadmap's "no silent caps" convention).
func (s *MemoryStore) Prune(ctx context.Context, scopes []string, tenantID string, halfLifeDays, threshold float64, asOf time.Time) ([]string, error) {
	active, err := s.ListActive(ctx, scopes, tenantID)
	if err != nil {
		return nil, err
	}

	var pruned []string
	for _, rec := range active {
		since := rec.CreatedAt
		if rec.LastUsedAt != nil {
			since = *rec.LastUsedAt
		}
		if DecayedUtility(rec.UtilityScore, since, asOf, halfLifeDays) >= threshold {
			continue
		}
		if _, err := s.transitionStatus(ctx, rec.MemoryID, MemoryStatusRevoked, memoryPostActiveTransitions); err != nil {
			return nil, err
		}
		pruned = append(pruned, rec.MemoryID)
	}
	return pruned, nil
}
