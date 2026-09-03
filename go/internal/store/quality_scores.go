package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// QualityScore is the current real eval score for one (provider, model) pair (MDL-002).
type QualityScore struct {
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Score     float64   `json:"score"`
	Suite     string    `json:"suite"`
	UpdatedAt time.Time `json:"updated_at"`
}

// QualityScoreStore is the Postgres-backed quality score store, and also — by structurally
// implementing modelgateway.QualityGate's IsDegraded(ctx, provider, model) bool method, with no
// import from this package to that one — the real routing input MDL-002 wires into
// modelgateway.Gateway.Quality.
type QualityScoreStore struct {
	pool      *pgxpool.Pool
	threshold float64
}

// Report upserts the current score for (provider, model) — the real write path an eval suite
// (provider_conformance, or any future one) uses to report its result. A later report for the same
// pair replaces the earlier one; no history is kept (see backlog.md).
func (q *QualityScoreStore) Report(ctx context.Context, provider, model, suite string, score float64) error {
	_, err := q.pool.Exec(ctx,
		`INSERT INTO model_quality_scores (provider, model, score, suite, updated_at)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (provider, model) DO UPDATE SET score = $3, suite = $4, updated_at = now()`,
		provider, model, score, suite,
	)
	if err != nil {
		return fmt.Errorf("store: reporting quality score: %w", err)
	}
	return nil
}

// Get fetches the current score for (provider, model), or ErrNotFound if none has ever been
// reported.
func (q *QualityScoreStore) Get(ctx context.Context, provider, model string) (*QualityScore, error) {
	row := q.pool.QueryRow(ctx,
		`SELECT provider, model, score, suite, updated_at FROM model_quality_scores WHERE provider = $1 AND model = $2`,
		provider, model,
	)
	var s QualityScore
	if err := row.Scan(&s.Provider, &s.Model, &s.Score, &s.Suite, &s.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: fetching quality score: %w", err)
	}
	return &s, nil
}

// List returns every currently-reported quality score.
func (q *QualityScoreStore) List(ctx context.Context) ([]QualityScore, error) {
	rows, err := q.pool.Query(ctx, `SELECT provider, model, score, suite, updated_at FROM model_quality_scores ORDER BY provider, model`)
	if err != nil {
		return nil, fmt.Errorf("store: listing quality scores: %w", err)
	}
	defer rows.Close()

	var out []QualityScore
	for rows.Next() {
		var s QualityScore
		if err := rows.Scan(&s.Provider, &s.Model, &s.Score, &s.Suite, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning quality score: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// IsDegraded reports whether (provider, model)'s currently-reported score is below this store's
// configured threshold. A pair that has never been scored is NOT degraded — absence of eval data
// must never block routing on its own, only an explicitly low score should (a real eval suite
// simply hasn't run against it yet, which is the common case for most candidates most of the time).
func (q *QualityScoreStore) IsDegraded(ctx context.Context, provider, model string) bool {
	score, err := q.Get(ctx, provider, model)
	if err != nil {
		return false
	}
	return score.Score < q.threshold
}
