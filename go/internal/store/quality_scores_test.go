package store

import (
	"context"
	"errors"
	"testing"
)

func TestQualityScoreStoreReportThenGet(t *testing.T) {
	s := newTestStore(t)
	scores := s.QualityScores(0.9)
	ctx := context.Background()
	model := "test-model-" + randSuffix(t)

	if err := scores.Report(ctx, "anthropic", model, "provider_conformance", 0.95); err != nil {
		t.Fatalf("Report: %v", err)
	}

	got, err := scores.Get(ctx, "anthropic", model)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Score != 0.95 || got.Suite != "provider_conformance" {
		t.Fatalf("unexpected score: %+v", got)
	}
}

func TestQualityScoreStoreReportReplacesEarlierScore(t *testing.T) {
	s := newTestStore(t)
	scores := s.QualityScores(0.9)
	ctx := context.Background()
	model := "test-model-" + randSuffix(t)

	if err := scores.Report(ctx, "anthropic", model, "provider_conformance", 0.95); err != nil {
		t.Fatalf("Report (1st): %v", err)
	}
	if err := scores.Report(ctx, "anthropic", model, "provider_conformance", 0.40); err != nil {
		t.Fatalf("Report (2nd): %v", err)
	}

	got, err := scores.Get(ctx, "anthropic", model)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Score != 0.40 {
		t.Fatalf("Score = %v, want the latest report (0.40)", got.Score)
	}
}

func TestQualityScoreStoreGetUnknownPairIsNotFound(t *testing.T) {
	s := newTestStore(t)
	scores := s.QualityScores(0.9)
	if _, err := scores.Get(context.Background(), "anthropic", "does-not-exist-"+randSuffix(t)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// TestQualityScoreStoreIsDegradedRealAcceptance is MDL-002's storage-layer proof: a real score
// below the configured threshold reports as degraded; a real score at or above it does not; and a
// pair with no score reported at all is never treated as degraded (absence of data must not block
// routing).
func TestQualityScoreStoreIsDegradedRealAcceptance(t *testing.T) {
	s := newTestStore(t)
	scores := s.QualityScores(0.9)
	ctx := context.Background()

	degradedModel := "degraded-model-" + randSuffix(t)
	healthyModel := "healthy-model-" + randSuffix(t)
	unscoredModel := "unscored-model-" + randSuffix(t)

	if err := scores.Report(ctx, "anthropic", degradedModel, "provider_conformance", 0.5); err != nil {
		t.Fatalf("Report (degraded): %v", err)
	}
	if err := scores.Report(ctx, "anthropic", healthyModel, "provider_conformance", 0.95); err != nil {
		t.Fatalf("Report (healthy): %v", err)
	}

	if !scores.IsDegraded(ctx, "anthropic", degradedModel) {
		t.Error("expected the low-scoring model to be degraded")
	}
	if scores.IsDegraded(ctx, "anthropic", healthyModel) {
		t.Error("expected the high-scoring model to not be degraded")
	}
	if scores.IsDegraded(ctx, "anthropic", unscoredModel) {
		t.Error("expected an unscored model to not be degraded — absence of data must not block routing")
	}
}

func TestQualityScoreStoreListReturnsEveryScore(t *testing.T) {
	s := newTestStore(t)
	scores := s.QualityScores(0.9)
	ctx := context.Background()
	model := "test-model-" + randSuffix(t)

	if err := scores.Report(ctx, "openai", model, "provider_conformance", 0.8); err != nil {
		t.Fatalf("Report: %v", err)
	}
	all, err := scores.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, s := range all {
		if s.Provider == "openai" && s.Model == model {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the reported score to appear in List, got %+v", all)
	}
}
