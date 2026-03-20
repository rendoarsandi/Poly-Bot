package main

import (
	"errors"
	"math"
	"testing"

	"Market-bot/internal/api"
)

func TestMergeablePairsAllowsDecimalPairs(t *testing.T) {
	got := mergeablePairs([]float64{0.966094, 1.250000})
	if math.Abs(got-0.966094) > 1e-9 {
		t.Fatalf("expected decimal balanced pairs to be mergeable, got %.6f", got)
	}

	if got := mergeablePairs([]float64{0.0099, 1}); got != 0 {
		t.Fatalf("expected sub-minimum balance to be ignored, got %.9f", got)
	}

	if got := mergeablePairs([]float64{0.01, 1}); math.Abs(got-0.01) > 1e-9 {
		t.Fatalf("expected 0.01 shares to remain mergeable, got %.9f", got)
	}
}

func TestIsSkippableRedeemError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "resolved pending", err: errors.New("market not yet resolved on-chain"), want: true},
		{name: "evm revert", err: errors.New("execution reverted"), want: true},
		{name: "payouts missing", err: errors.New("payouts not reported yet"), want: true},
		{name: "other error", err: errors.New("network timeout"), want: false},
	}

	for _, tt := range tests {
		if got := isSkippableRedeemError(tt.err); got != tt.want {
			t.Fatalf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}
}

func TestAutoRedeemDecisionSkipsUndecidedMarkets(t *testing.T) {
	info := &api.MarketInfo{Closed: false}
	if winner, ok := autoRedeemDecision(info, []string{"Up", "Down"}, []float64{1, 0}); ok || winner != "" {
		t.Fatalf("expected unresolved market to skip redeem, got winner=%q ok=%v", winner, ok)
	}

	info = &api.MarketInfo{
		Closed: true,
		Tokens: []struct {
			TokenID string      `json:"token_id"`
			Outcome string      `json:"outcome"`
			Winner  bool        `json:"winner"`
			Price   interface{} `json:"price"`
		}{
			{Outcome: "Up", Winner: false},
			{Outcome: "Down", Winner: false},
		},
	}
	if winner, ok := autoRedeemDecision(info, []string{"Up", "Down"}, []float64{1, 0}); ok || winner != "" {
		t.Fatalf("expected no-winner market to skip redeem, got winner=%q ok=%v", winner, ok)
	}
}

func TestAutoRedeemDecisionRedeemsOnlyWinningShares(t *testing.T) {
	info := &api.MarketInfo{
		Closed: true,
		Tokens: []struct {
			TokenID string      `json:"token_id"`
			Outcome string      `json:"outcome"`
			Winner  bool        `json:"winner"`
			Price   interface{} `json:"price"`
		}{
			{Outcome: "Up", Winner: true},
			{Outcome: "Down", Winner: false},
		},
	}

	if winner, ok := autoRedeemDecision(info, []string{"Up", "Down"}, []float64{0.5, 1.0}); !ok || winner != "Up" {
		t.Fatalf("expected winning balance to auto redeem, got winner=%q ok=%v", winner, ok)
	}

	if winner, ok := autoRedeemDecision(info, []string{"Up", "Down"}, []float64{0, 1.0}); ok || winner != "Up" {
		t.Fatalf("expected only losing balance to skip redeem, got winner=%q ok=%v", winner, ok)
	}
}
