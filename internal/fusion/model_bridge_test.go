package fusion

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApplyExternalModelScoreBlendsIntoFeatures(t *testing.T) {
	features := ModelFeatures{CurrentProb: 0.55, FairUp: 0.61, Score: 0.06, PrimaryReason: "flow"}
	updated := applyExternalModelScore(features, externalModelScore{Score: 0.18, Confidence: 0.8, Reason: "xgb"})
	if updated.Score <= features.Score {
		t.Fatalf("expected blended score to increase, before=%+v after=%+v", features, updated)
	}
	if updated.FairUp <= features.FairUp {
		t.Fatalf("expected fair value to increase, before=%+v after=%+v", features, updated)
	}
	if updated.ExternalModelWeight <= 0 || updated.ExternalModelReason != "xgb" {
		t.Fatalf("expected external model metadata to be populated, got %+v", updated)
	}
}

func TestExternalScoreProviderLoadsWrappedJSON(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "scores.json")
	content := `{"updated_at":"2026-03-09T07:00:00Z","scores":{"BTC":{"score":0.12,"confidence":0.7,"reason":"xgb"}}}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write temp scores: %v", err)
	}
	provider := newExternalScoreProvider(path)
	score, ok := provider.Lookup("btc")
	if !ok {
		t.Fatal("expected BTC score to load")
	}
	if score.Score != 0.12 || score.Confidence != 0.7 || score.Reason != "xgb" {
		t.Fatalf("unexpected score payload: %+v", score)
	}
	if score.UpdatedAt.IsZero() || !score.UpdatedAt.Equal(time.Date(2026, 3, 9, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected updated_at: %+v", score)
	}
}

func TestExternalScoreProviderLookupFreshRejectsStaleScores(t *testing.T) {
	provider := &externalScoreProvider{scores: map[string]externalModelScore{
		"BTC": {Score: 0.10, Confidence: 0.8, UpdatedAt: time.Date(2026, 3, 9, 7, 0, 0, 0, time.UTC)},
	}}
	if _, ok := provider.LookupFresh("BTC", 30*time.Second, time.Date(2026, 3, 9, 7, 0, 20, 0, time.UTC)); !ok {
		t.Fatal("expected fresh score to be accepted")
	}
	if _, ok := provider.LookupFresh("BTC", 30*time.Second, time.Date(2026, 3, 9, 7, 1, 0, 0, time.UTC)); ok {
		t.Fatal("expected stale score to be rejected")
	}
}
