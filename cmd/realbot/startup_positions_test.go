package main

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

type stubRealbotStartupMarketInfoReader struct {
	infos map[string]*api.MarketInfo
	errs  map[string]error
	calls map[string]int
}

type stubRealbotStartupCarryBalanceReader struct {
	address  string
	balances map[string]float64
	errs     map[string]error
}

func (s *stubRealbotStartupCarryBalanceReader) Address() string {
	return s.address
}

func (s *stubRealbotStartupCarryBalanceReader) GetCTFBalanceFloat(ctx context.Context, tokenID string) (float64, error) {
	_ = ctx
	if err := s.errs[tokenID]; err != nil {
		return 0, err
	}
	return s.balances[tokenID], nil
}

type stubRealbotStartupCarryRecoverySource struct {
	marketsByTimeframe map[string][]api.Market
	marketErrs         map[string]error
	tradesByOffset     map[int][]api.PublicTrade
	tradeErrs          map[int]error
}

func (s *stubRealbotStartupCarryRecoverySource) GetMarketsByTimeframe(ctx context.Context, assets []string, timeframe string) ([]api.Market, error) {
	_ = ctx
	_ = assets
	if err := s.marketErrs[timeframe]; err != nil {
		return nil, err
	}
	return append([]api.Market(nil), s.marketsByTimeframe[timeframe]...), nil
}

func (s *stubRealbotStartupCarryRecoverySource) GetPublicTradesPage(ctx context.Context, user string, markets []string, limit int, offset int) ([]api.PublicTrade, error) {
	_ = ctx
	_ = user
	_ = markets
	_ = limit
	if err := s.tradeErrs[offset]; err != nil {
		return nil, err
	}
	return append([]api.PublicTrade(nil), s.tradesByOffset[offset]...), nil
}

func (s *stubRealbotStartupMarketInfoReader) GetMarketInfo(ctx context.Context, conditionID string) (*api.MarketInfo, error) {
	_ = ctx
	if s.calls == nil {
		s.calls = make(map[string]int)
	}
	s.calls[conditionID]++
	if err := s.errs[conditionID]; err != nil {
		return nil, err
	}
	if info := s.infos[conditionID]; info != nil {
		return info, nil
	}
	return nil, nil
}

func realbotTestMarketInfo(closed bool, winner string, outcomes ...string) *api.MarketInfo {
	info := &api.MarketInfo{Closed: closed}
	for _, outcome := range outcomes {
		info.Tokens = append(info.Tokens, struct {
			TokenID string      `json:"token_id"`
			Outcome string      `json:"outcome"`
			Winner  bool        `json:"winner"`
			Price   interface{} `json:"price"`
		}{
			TokenID: outcome + "-token",
			Outcome: outcome,
			Winner:  outcome == winner,
		})
	}
	return info
}

func TestRealbotFilterStartupCarryPositionsSkipsResolvedLosers(t *testing.T) {
	reader := &stubRealbotStartupMarketInfoReader{
		infos: map[string]*api.MarketInfo{
			"cond-1": realbotTestMarketInfo(true, "Up", "Up", "Down"),
			"cond-2": realbotTestMarketInfo(false, "", "Yes", "No"),
		},
	}

	positions := []trading.PositionInfo{
		{ConditionID: "cond-1", Outcome: "Up", Size: 5, AvgPrice: 0.61},
		{ConditionID: "cond-1", Outcome: "Down", Size: 8, AvgPrice: 0.29},
		{ConditionID: "cond-2", Outcome: "Yes", Size: 3, AvgPrice: 0.44},
	}

	filtered, skippedPositions, skippedShares := realbotFilterStartupCarryPositions(context.Background(), reader, positions)
	if skippedPositions != 1 {
		t.Fatalf("expected 1 skipped startup position, got %d", skippedPositions)
	}
	if math.Abs(skippedShares-8) > 0.000001 {
		t.Fatalf("expected 8 skipped shares, got %.6f", skippedShares)
	}
	if len(filtered) != 2 {
		t.Fatalf("expected 2 filtered startup positions, got %d", len(filtered))
	}
	if filtered[0].Outcome != "Up" || filtered[1].Outcome != "Yes" {
		t.Fatalf("unexpected filtered outcomes: %+v", filtered)
	}
	if reader.calls["cond-1"] != 1 || reader.calls["cond-2"] != 1 {
		t.Fatalf("expected one market-info lookup per condition, got %+v", reader.calls)
	}
}

func TestRealbotFilterStartupCarryPositionsKeepsPositionsWhenWinnerUnavailable(t *testing.T) {
	reader := &stubRealbotStartupMarketInfoReader{
		infos: map[string]*api.MarketInfo{
			"cond-pending": realbotTestMarketInfo(true, "", "Up", "Down"),
		},
		errs: map[string]error{
			"cond-error": errors.New("lookup failed"),
		},
	}

	positions := []trading.PositionInfo{
		{ConditionID: "cond-pending", Outcome: "Down", Size: 4, AvgPrice: 0.40},
		{ConditionID: "cond-error", Outcome: "Yes", Size: 2, AvgPrice: 0.55},
	}

	filtered, skippedPositions, skippedShares := realbotFilterStartupCarryPositions(context.Background(), reader, positions)
	if skippedPositions != 0 || skippedShares != 0 {
		t.Fatalf("expected unresolved or failed lookup positions to stay loaded, got skipped=%d shares=%.6f", skippedPositions, skippedShares)
	}
	if len(filtered) != len(positions) {
		t.Fatalf("expected all startup positions to remain, got %+v", filtered)
	}
}

func TestStartupCarryImportSkipsResolvedLosersFromExposure(t *testing.T) {
	reader := &stubRealbotStartupMarketInfoReader{
		infos: map[string]*api.MarketInfo{
			"cond-loss": realbotTestMarketInfo(true, "Up", "Up", "Down"),
			"cond-open": realbotTestMarketInfo(false, "", "Yes", "No"),
		},
	}

	positions := []trading.PositionInfo{
		{ConditionID: "cond-loss", Outcome: "Down", Size: 12, AvgPrice: 0.38},
		{ConditionID: "cond-open", Outcome: "Yes", Size: 4, AvgPrice: 0.45},
	}

	filtered, skippedPositions, _ := realbotFilterStartupCarryPositions(context.Background(), reader, positions)
	if skippedPositions != 1 {
		t.Fatalf("expected resolved loser to be skipped, got %d skipped positions", skippedPositions)
	}

	engine := paper.NewEngine(100)
	for _, pos := range filtered {
		engine.SyncExternalPosition(realbotStartupCarryMarketID(pos), pos.Outcome, pos.Size, pos.AvgPrice)
	}

	totalExposure, _ := engine.GetExposure()
	if math.Abs(totalExposure-1.8) > 0.000001 {
		t.Fatalf("expected only unresolved carry to count toward exposure (1.80), got %.6f", totalExposure)
	}
}

func TestRealbotFilterDustStartupCarryPositions(t *testing.T) {
	positions := []trading.PositionInfo{
		{ConditionID: "cond-dust", Outcome: "Up", Size: 0.009, AvgPrice: 0.61},
		{ConditionID: "cond-keep", Outcome: "Down", Size: 0.015, AvgPrice: 0.29},
	}

	filtered, skippedPositions, skippedShares := realbotFilterDustStartupCarryPositions(positions)
	if skippedPositions != 1 {
		t.Fatalf("expected 1 skipped dust startup position, got %d", skippedPositions)
	}
	if math.Abs(skippedShares-0.009) > 0.000001 {
		t.Fatalf("expected 0.009 skipped dust shares, got %.6f", skippedShares)
	}
	if len(filtered) != 1 || filtered[0].ConditionID != "cond-keep" {
		t.Fatalf("unexpected filtered startup carry positions: %+v", filtered)
	}
}

func TestRealbotStartupCarryMarketIDPrefersSlug(t *testing.T) {
	pos := trading.PositionInfo{
		ConditionID: "0xc55fe114eac7a91fc5c0bbe248f65b8bf863aad990785db8bd04d8022cb0c08c",
		Slug:        "btc-updown-5m-1776383100",
	}

	if got := realbotStartupCarryMarketID(pos); got != "btc-updown-5m-1776383100" {
		t.Fatalf("expected startup carry market id to prefer slug, got %q", got)
	}
}

func TestRealbotStartupCarryMarketsBuildsCanonicalMetadata(t *testing.T) {
	reader := &stubRealbotStartupMarketInfoReader{
		infos: map[string]*api.MarketInfo{
			"cond-1": {
				Closed:     true,
				EndDateISO: "2026-04-17T00:00:00Z",
				Tokens: []struct {
					TokenID string      `json:"token_id"`
					Outcome string      `json:"outcome"`
					Winner  bool        `json:"winner"`
					Price   interface{} `json:"price"`
				}{
					{TokenID: "up-token", Outcome: "Up"},
					{TokenID: "down-token", Outcome: "Down"},
				},
			},
		},
	}

	positions := []trading.PositionInfo{
		{
			ConditionID:     "cond-1",
			Slug:            "btc-updown-5m-1776383100",
			Outcome:         "Up",
			OppositeOutcome: "Down",
			Size:            1.0163,
		},
	}

	markets := realbotStartupCarryMarkets(context.Background(), reader, positions)
	carry, ok := markets["btc-updown-5m-1776383100"]
	if !ok {
		t.Fatalf("expected canonical startup carry market keyed by slug, got %+v", markets)
	}
	if carry.ConditionID != "cond-1" {
		t.Fatalf("expected condition id cond-1, got %+v", carry)
	}
	if len(carry.Outcomes) != 2 || carry.Outcomes[0] != "Down" || carry.Outcomes[1] != "Up" {
		t.Fatalf("expected canonical outcomes from startup carry metadata, got %+v", carry.Outcomes)
	}
	wantEnd := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)
	if !carry.EndTime.Equal(wantEnd) {
		t.Fatalf("expected startup carry end time %s, got %s", wantEnd, carry.EndTime)
	}
}

func TestRealbotStartupCarryAvgPriceFromTradesUsesAverageCostMath(t *testing.T) {
	trades := []api.PublicTrade{
		{Asset: "up-token", Side: "BUY", Size: 10, Price: 0.20, Timestamp: 1},
		{Asset: "up-token", Side: "BUY", Size: 10, Price: 0.80, Timestamp: 2},
		{Asset: "up-token", Side: "SELL", Size: 10, Price: 0.90, Timestamp: 3},
	}

	avgPrice, ok := realbotStartupCarryAvgPriceFromTrades(trades, 10)
	if !ok {
		t.Fatal("expected average-cost reconstruction to succeed")
	}
	if math.Abs(avgPrice-0.50) > 0.000001 {
		t.Fatalf("expected remaining average cost 0.50, got %.6f", avgPrice)
	}
}

func TestRealbotRecoverStartupCarryPositionsRestoresClosedCarryFromWalletScan(t *testing.T) {
	trader := &stubRealbotStartupCarryBalanceReader{
		address: "0xe0229e10a858860218b6132f4234602c47bd6603",
		balances: map[string]float64{
			"up-token": 1.0163,
		},
	}
	source := &stubRealbotStartupCarryRecoverySource{
		marketsByTimeframe: map[string][]api.Market{
			"5m": {
				{
					ConditionID: "cond-1",
					Slug:        "btc-updown-5m-1776400200",
					Tokens: []api.Token{
						{TokenID: "up-token", Outcome: "Up"},
						{TokenID: "down-token", Outcome: "Down"},
					},
				},
			},
		},
		tradesByOffset: map[int][]api.PublicTrade{
			0: {
				{
					Asset:       "up-token",
					ConditionID: "cond-1",
					Slug:        "btc-updown-5m-1776400200",
					Side:        "BUY",
					Size:        1.0163,
					Price:       0.95,
					Timestamp:   10,
					Outcome:     "Up",
				},
			},
		},
	}

	positions, recovered, err := realbotRecoverStartupCarryPositions(context.Background(), trader, source, nil)
	if err != nil {
		t.Fatalf("expected startup carry recovery to succeed, got %v", err)
	}
	if recovered != 1 {
		t.Fatalf("expected 1 recovered startup carry position, got %d", recovered)
	}
	if len(positions) != 1 {
		t.Fatalf("expected 1 recovered position, got %+v", positions)
	}

	pos := positions[0]
	if pos.TokenID != "up-token" || pos.ConditionID != "cond-1" || pos.Slug != "btc-updown-5m-1776400200" {
		t.Fatalf("unexpected recovered startup position identity: %+v", pos)
	}
	if pos.Outcome != "Up" || pos.OppositeOutcome != "Down" || pos.OppositeAsset != "down-token" {
		t.Fatalf("unexpected recovered startup outcomes: %+v", pos)
	}
	if math.Abs(pos.Size-1.0163) > 0.000001 {
		t.Fatalf("expected 1.0163 recovered shares, got %.6f", pos.Size)
	}
	if math.Abs(pos.AvgPrice-0.95) > 0.000001 {
		t.Fatalf("expected recovered startup avg price 0.95, got %.6f", pos.AvgPrice)
	}
}

func TestRealbotStartupCarryScanMarketsIncludesOneHourBuckets(t *testing.T) {
	source := &stubRealbotStartupCarryRecoverySource{
		marketsByTimeframe: map[string][]api.Market{
			"1h": {
				{
					ConditionID: "cond-1h",
					Slug:        "btc-updown-1h-1776400200",
					Tokens: []api.Token{
						{TokenID: "up-token", Outcome: "Up"},
						{TokenID: "down-token", Outcome: "Down"},
					},
				},
			},
		},
	}

	markets, err := realbotStartupCarryScanMarkets(context.Background(), source)
	if err != nil {
		t.Fatalf("expected one-hour startup scan to succeed, got %v", err)
	}
	if len(markets) != 1 {
		t.Fatalf("expected 1 one-hour startup market, got %+v", markets)
	}
	if markets[0].ConditionID != "cond-1h" || markets[0].Slug != "btc-updown-1h-1776400200" {
		t.Fatalf("unexpected one-hour startup market: %+v", markets[0])
	}
}
