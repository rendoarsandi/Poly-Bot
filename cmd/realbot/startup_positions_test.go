package main

import (
	"context"
	"errors"
	"math"
	"testing"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
	"Market-bot/internal/trading"
)

type stubRealbotStartupMarketInfoReader struct {
	infos map[string]*api.MarketInfo
	errs  map[string]error
	calls map[string]int
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
		engine.SyncExternalPosition(pos.ConditionID, pos.Outcome, pos.Size, pos.AvgPrice)
	}

	totalExposure, _ := engine.GetExposure()
	if math.Abs(totalExposure-1.8) > 0.000001 {
		t.Fatalf("expected only unresolved carry to count toward exposure (1.80), got %.6f", totalExposure)
	}
}
