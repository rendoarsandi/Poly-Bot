package fusion

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"Market-bot/internal/core"
	"Market-bot/internal/paper"
)

type ReplaySnapshot struct {
	Timestamp            time.Time     `json:"timestamp"`
	Asset                string        `json:"asset"`
	MarketID             string        `json:"market_id"`
	MarketSlug           string        `json:"market_slug"`
	UpBid                float64       `json:"up_bid"`
	UpAsk                float64       `json:"up_ask"`
	DownBid              float64       `json:"down_bid"`
	DownAsk              float64       `json:"down_ask"`
	TimeRemainingSec     float64       `json:"time_remaining_sec"`
	MarketDataAgeMillis  int64         `json:"market_data_age_ms"`
	BinanceDataAgeMillis int64         `json:"binance_data_age_ms"`
	UpAskDepth           float64       `json:"up_ask_depth"`
	DownAskDepth         float64       `json:"down_ask_depth"`
	Features             ModelFeatures `json:"features"`
	Decision             Decision      `json:"decision"`
}

type ReplayTrade struct {
	Asset       string    `json:"asset"`
	MarketID    string    `json:"market_id"`
	Outcome     string    `json:"outcome"`
	EntryAt     time.Time `json:"entry_at"`
	ExitAt      time.Time `json:"exit_at"`
	EntryPrice  float64   `json:"entry_price"`
	ExitPrice   float64   `json:"exit_price"`
	Quantity    float64   `json:"quantity"`
	PnL         float64   `json:"pnl"`
	ModelWeight float64   `json:"model_weight"`
	ModelReason string    `json:"model_reason"`
	Reason      string    `json:"reason"`
}

type ReplayAssetReport struct {
	Trades      int     `json:"trades"`
	Winning     int     `json:"winning"`
	RealizedPnL float64 `json:"realized_pnl"`
	WinRate     float64 `json:"win_rate"`
}

type ReplayReport struct {
	Snapshots          int                          `json:"snapshots"`
	CompletedTrades    int                          `json:"completed_trades"`
	RealizedPnL        float64                      `json:"realized_pnl"`
	FinalEquity        float64                      `json:"final_equity"`
	MaxDrawdownPct     float64                      `json:"max_drawdown_pct"`
	WinRate            float64                      `json:"win_rate"`
	ModelSnapshots     int                          `json:"model_snapshots"`
	ModelTrades        int                          `json:"model_trades"`
	AverageModelWeight float64                      `json:"average_model_weight"`
	ByAsset            map[string]ReplayAssetReport `json:"by_asset"`
	Trades             []ReplayTrade                `json:"trades"`
}

type snapshotRecorder struct {
	mu   sync.Mutex
	file *os.File
}

func newSnapshotRecorder(path string) (*snapshotRecorder, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil && filepath.Dir(path) != "." {
		return nil, fmt.Errorf("mkdir recorder dir: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open recorder file: %w", err)
	}
	return &snapshotRecorder{file: file}, nil
}

func (r *snapshotRecorder) Record(snapshot ReplaySnapshot) error {
	if r == nil || r.file == nil {
		return nil
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err = r.file.Write(append(data, '\n'))
	return err
}

func (r *snapshotRecorder) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	return r.file.Close()
}

func LoadReplaySnapshots(path string) ([]ReplaySnapshot, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	snapshots := make([]ReplaySnapshot, 0, 256)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var snapshot ReplaySnapshot
		if err := json.Unmarshal([]byte(line), &snapshot); err != nil {
			return nil, fmt.Errorf("parse replay line: %w", err)
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return snapshots, nil
}

func EvaluateReplay(cfg *core.Config, snapshots []ReplaySnapshot) ReplayReport {
	if cfg == nil {
		cfg = defaultFusionConfig()
	}
	startingBalance := cfg.BaseBalance
	if startingBalance <= 0 {
		startingBalance = defaultStartingBalance
	}
	engine := paper.NewEngine(startingBalance)
	engine.SetFeeRateBps(cfg.FeeRateBps)

	sorted := append([]ReplaySnapshot(nil), snapshots...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Timestamp.Before(sorted[j].Timestamp) })

	byAsset := map[string]*ReplayAssetReport{}
	openEntries := map[string]ReplayTrade{}
	completed := make([]ReplayTrade, 0)
	modelSnapshots := 0
	modelWeightSum := 0.0

	for _, snapshot := range sorted {
		if snapshot.Features.ExternalModelWeight > 0 {
			modelSnapshots++
			modelWeightSum += snapshot.Features.ExternalModelWeight
		}
		engine.UpdateMarketBidAsk(snapshot.Asset, "Up", snapshot.UpBid, snapshot.UpAsk)
		engine.UpdateMarketBidAsk(snapshot.Asset, "Down", snapshot.DownBid, snapshot.DownAsk)
		pos := replayPositionForAsset(engine, snapshot.Asset)
		decision := decideAction(cfg, snapshot.toSignalSnapshot(pos))

		switch decision.Action {
		case "BUY":
			if pos != nil && !strings.EqualFold(pos.Outcome, decision.Outcome) {
				continue
			}
			qty := decisionQuantity(cfg, engine.GetEquity(), decision, snapshot.askDepth(decision.Outcome))
			if qty < 1 {
				continue
			}
			if _, err := engine.BuyForMarket(snapshot.Asset, decision.Outcome, decision.Price, qty); err != nil {
				continue
			}
			entry := openEntries[snapshot.Asset]
			if entry.EntryAt.IsZero() {
				openEntries[snapshot.Asset] = ReplayTrade{Asset: snapshot.Asset, MarketID: snapshot.MarketID, Outcome: decision.Outcome, EntryAt: snapshot.Timestamp, Reason: decision.Reason, ModelWeight: snapshot.Features.ExternalModelWeight, ModelReason: snapshot.Features.ExternalModelReason}
			}
		case "SELL":
			if pos == nil || !strings.EqualFold(pos.Outcome, decision.Outcome) {
				continue
			}
			quantity := pos.Quantity
			entry := openEntries[snapshot.Asset]
			if _, err := engine.SellForMarket(snapshot.Asset, decision.Outcome, decision.Price, quantity); err != nil {
				continue
			}
			pnl := (decision.Price - pos.AvgPrice) * quantity
			assetReport := byAsset[snapshot.Asset]
			if assetReport == nil {
				assetReport = &ReplayAssetReport{}
				byAsset[snapshot.Asset] = assetReport
			}
			assetReport.Trades++
			assetReport.RealizedPnL += pnl
			if pnl > 0 {
				assetReport.Winning++
			}
			completed = append(completed, ReplayTrade{
				Asset:       snapshot.Asset,
				MarketID:    snapshot.MarketID,
				Outcome:     decision.Outcome,
				EntryAt:     entry.EntryAt,
				ExitAt:      snapshot.Timestamp,
				EntryPrice:  pos.AvgPrice,
				ExitPrice:   decision.Price,
				Quantity:    quantity,
				PnL:         pnl,
				ModelWeight: entry.ModelWeight,
				ModelReason: entry.ModelReason,
				Reason:      decision.Reason,
			})
			delete(openEntries, snapshot.Asset)
		}
	}

	stats := engine.GetStats()
	report := ReplayReport{
		Snapshots:       len(sorted),
		CompletedTrades: len(completed),
		RealizedPnL:     stats.RealizedPnL,
		FinalEquity:     engine.GetEquity(),
		MaxDrawdownPct:  stats.MaxDrawdown,
		ModelSnapshots:  modelSnapshots,
		ByAsset:         map[string]ReplayAssetReport{},
		Trades:          completed,
	}
	winning := 0
	modelTrades := 0
	for asset, item := range byAsset {
		if item.Trades > 0 {
			item.WinRate = float64(item.Winning) / float64(item.Trades)
		}
		winning += item.Winning
		report.ByAsset[asset] = *item
	}
	if report.CompletedTrades > 0 {
		report.WinRate = float64(winning) / float64(report.CompletedTrades)
	}
	for _, trade := range completed {
		if trade.ModelWeight > 0 {
			modelTrades++
		}
	}
	report.ModelTrades = modelTrades
	if modelSnapshots > 0 {
		report.AverageModelWeight = modelWeightSum / float64(modelSnapshots)
	}
	return report
}

func decisionQuantity(cfg *core.Config, equity float64, decision Decision, availableAskDepth float64) float64 {
	if decision.Action != "BUY" || decision.Price <= 0 {
		return 0
	}
	tradeBudget := cfg.CalculateTradeSize(equity) * decision.Confidence
	qty := roundedShares(tradeBudget / decision.Price)
	if availableAskDepth > 0 {
		depthCap := roundedShares(availableAskDepth * 0.25)
		if depthCap > 0 && qty > depthCap {
			qty = depthCap
		}
	}
	return qty
}

func replayPositionForAsset(engine *paper.Engine, asset string) *paper.Position {
	positions := engine.GetPositions()
	for _, outcome := range []string{"Up", "Down"} {
		if pos, ok := positions[asset+":"+outcome]; ok && pos.Quantity > 0 {
			copyPos := pos
			return &copyPos
		}
	}
	return nil
}

func (snapshot ReplaySnapshot) toSignalSnapshot(pos *paper.Position) SignalSnapshot {
	return SignalSnapshot{
		Asset:          snapshot.Asset,
		UpBid:          snapshot.UpBid,
		UpAsk:          snapshot.UpAsk,
		DownBid:        snapshot.DownBid,
		DownAsk:        snapshot.DownAsk,
		TimeRemaining:  time.Duration(snapshot.TimeRemainingSec * float64(time.Second)),
		Position:       pos,
		Features:       snapshot.Features,
		MarketDataAge:  time.Duration(snapshot.MarketDataAgeMillis) * time.Millisecond,
		BinanceDataAge: time.Duration(snapshot.BinanceDataAgeMillis) * time.Millisecond,
		UpAskDepth:     snapshot.UpAskDepth,
		DownAskDepth:   snapshot.DownAskDepth,
	}
}

func (snapshot ReplaySnapshot) askDepth(outcome string) float64 {
	if strings.EqualFold(outcome, "Up") {
		return snapshot.UpAskDepth
	}
	return snapshot.DownAskDepth
}
