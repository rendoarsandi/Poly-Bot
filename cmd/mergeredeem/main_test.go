package main

import (
	"context"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"Market-bot/internal/api"
)

type stubMarketInfoFetcher struct {
	info *api.MarketInfo
	err  error
}

func (s stubMarketInfoFetcher) GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error) {
	return s.info, s.err
}

type stubMarketResolutionReader struct {
	resolved   bool
	winner     string
	resolveErr error
	winnerErr  error
	numerators []*big.Int
}

func (s stubMarketResolutionReader) IsMarketResolved(ctx context.Context, conditionID string) (bool, error) {
	return s.resolved, s.resolveErr
}

func (s stubMarketResolutionReader) GetWinningOutcome(ctx context.Context, conditionID string, outcomes []string) (string, error) {
	return s.winner, s.winnerErr
}

func (s stubMarketResolutionReader) GetPayoutNumerator(ctx context.Context, conditionID string, index int) (*big.Int, error) {
	if index >= 0 && index < len(s.numerators) {
		return s.numerators[index], nil
	}
	// Fallback when numerators are not set in the stub
	if s.winner != "" {
		for i, outcome := range []string{"Up", "Down"} {
			if outcome == s.winner && i == index {
				return big.NewInt(1), nil
			}
		}
	}
	return big.NewInt(0), nil
}

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

func TestResolveRedeemDecisionFallsBackToOnChainWinner(t *testing.T) {
	market := api.Market{
		ConditionID: "0xabc",
		EndTime:     time.Now().Add(-time.Minute),
		Tokens: []api.Token{
			{Outcome: "Up"},
			{Outcome: "Down"},
		},
	}
	infoFetcher := stubMarketInfoFetcher{
		info: &api.MarketInfo{Closed: false},
	}
	resolutionReader := stubMarketResolutionReader{
		resolved: true,
		winner:   "Up",
	}

	decision, err := resolveRedeemDecision(context.Background(), infoFetcher, resolutionReader, market, []float64{1, 0})
	if err != nil {
		t.Fatalf("resolveRedeemDecision() error = %v", err)
	}
	if !decision.shouldRedeem || decision.winnerOutcome != "Up" || decision.source != "on-chain" {
		t.Fatalf("expected on-chain winning redeem decision, got %+v", decision)
	}
}

func TestResolveRedeemDecisionSkipsLosingBalanceQuietly(t *testing.T) {
	market := api.Market{
		ConditionID: "0xabc",
		EndTime:     time.Now().Add(-time.Minute),
		Tokens: []api.Token{
			{Outcome: "Up"},
			{Outcome: "Down"},
		},
	}
	infoFetcher := stubMarketInfoFetcher{
		info: &api.MarketInfo{
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
		},
	}

	decision, err := resolveRedeemDecision(context.Background(), infoFetcher, stubMarketResolutionReader{}, market, []float64{0, 1})
	if err != nil {
		t.Fatalf("resolveRedeemDecision() error = %v", err)
	}
	if decision.shouldRedeem {
		t.Fatalf("expected losing-only balance to skip redeem, got %+v", decision)
	}
	if decision.reason != redeemSkipOnlyLosingBalance {
		t.Fatalf("expected losing-only skip reason, got %+v", decision)
	}
}

func TestResolveRedeemDecisionHandlesSplitOrTie(t *testing.T) {
	market := api.Market{
		ConditionID: "0xabc",
		EndTime:     time.Now().Add(-time.Minute),
		Tokens: []api.Token{
			{Outcome: "Up"},
			{Outcome: "Down"},
		},
	}
	infoFetcher := stubMarketInfoFetcher{
		info: &api.MarketInfo{Closed: false},
	}
	resolutionReader := stubMarketResolutionReader{
		resolved:   true,
		winner:     "",
		numerators: []*big.Int{big.NewInt(1), big.NewInt(1)}, // 50/50 payout
	}

	decision, err := resolveRedeemDecision(context.Background(), infoFetcher, resolutionReader, market, []float64{1, 0})
	if err != nil {
		t.Fatalf("resolveRedeemDecision() error = %v", err)
	}
	if !decision.shouldRedeem || decision.winnerOutcome != "Up/Down" || decision.source != "on-chain" {
		t.Fatalf("expected split/tie winning redeem decision, got %+v", decision)
	}
}

