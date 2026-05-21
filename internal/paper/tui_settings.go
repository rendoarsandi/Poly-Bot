package paper

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"Market-bot/internal/core"

	tea "github.com/charmbracelet/bubbletea"
)

// Preset quick-select settings.
var (
	SettingsConservative = TUISettings{Exchange: "polymarket", ExecutionBackend: core.ExecutionBackendPaper, MarketSlug: "ALL", MaxMarkets: 2, Timeframe: "15m", TradeSizingMode: core.TradeSizingModePercent, TradeScaleFactor: 0.01, TradeSizeUSDC: 1.0, MinMarginPercent: 3.0, BinanceSignalThresholdPct: 0.12, PaperBinanceExecutionDelayMs: 250, PaperArbMode: "taker", CopytradePollIntervalMs: 2000, CopytradeSizingMode: core.CopytradeSizingModeUSDC, CopytradeSizeUSDC: 1.0, CopytradeSizeShares: 1.0, CopytradeSizePercent: 100.0, CopytradeMaxSlippagePct: 1.0, LadderedTakerSizingMode: core.LadderedTakerSizingModeUSDC, LadderedTakerSizeUSDC: 1.0, LadderedTakerSizeShares: 1.0, LadderedTakerReentryMoveCents: 1.0, LadderedTakerMaxSlippagePct: 1.0, LadderedTakerPnLGuardMode: core.LadderedTakerPnLGuardWorst, LadderedTakerWorstPnLFloor: 0, LadderedTakerMaxProfitPnL: 0, BuyExecutionMarginFloorPercent: -0.01, SplitMinMarginSell: 5.0, MakerMergeBufferSeconds: 30, MakerQuoteGap: 0.008, MakerInventoryTargetMult: 3.0, MakerInventoryCapMult: 5.0, MakerMinQuoteValue: 5.0, MinAskPrice: 0.10, MaxAskPrice: 0.90, TradingHoursMode: "weekdays trade only", TakerCloseMarket: false, TakerCloseMarketTime: 5, TakerCloseMarketSlippage: 0.99, TakerCloseMarketMinPrice: 0.60, TakerCloseSizingMode: core.TakerCloseSizingModePercent, TakerCloseSizeUSDC: 1.0, TakerCloseSizeShares: 1.02}
	SettingsModerate     = TUISettings{Exchange: "polymarket", ExecutionBackend: core.ExecutionBackendPaper, MarketSlug: "ALL", MaxMarkets: 4, Timeframe: "15m", TradeSizingMode: core.TradeSizingModePercent, TradeScaleFactor: 0.05, TradeSizeUSDC: 5.0, MinMarginPercent: 2.0, BinanceSignalThresholdPct: 0.08, PaperBinanceExecutionDelayMs: 250, PaperArbMode: "taker", CopytradePollIntervalMs: 2000, CopytradeSizingMode: core.CopytradeSizingModeUSDC, CopytradeSizeUSDC: 5.0, CopytradeSizeShares: 5.0, CopytradeSizePercent: 100.0, CopytradeMaxSlippagePct: 1.0, LadderedTakerSizingMode: core.LadderedTakerSizingModeUSDC, LadderedTakerSizeUSDC: 5.0, LadderedTakerSizeShares: 5.0, LadderedTakerReentryMoveCents: 1.0, LadderedTakerMaxSlippagePct: 1.0, LadderedTakerPnLGuardMode: core.LadderedTakerPnLGuardWorst, LadderedTakerWorstPnLFloor: 0, LadderedTakerMaxProfitPnL: 0, BuyExecutionMarginFloorPercent: -0.01, SplitMinMarginSell: 3.0, MakerMergeBufferSeconds: 30, MakerQuoteGap: 0.008, MakerInventoryTargetMult: 3.0, MakerInventoryCapMult: 5.0, MakerMinQuoteValue: 5.0, MinAskPrice: 0.10, MaxAskPrice: 0.90, TradingHoursMode: "weekdays trade only", TakerCloseMarket: false, TakerCloseMarketTime: 5, TakerCloseMarketSlippage: 0.99, TakerCloseMarketMinPrice: 0.60, TakerCloseSizingMode: core.TakerCloseSizingModePercent, TakerCloseSizeUSDC: 1.0, TakerCloseSizeShares: 1.02}
	SettingsAggressive   = TUISettings{Exchange: "polymarket", ExecutionBackend: core.ExecutionBackendPaper, MarketSlug: "ALL", MaxMarkets: 4, Timeframe: "15m", TradeSizingMode: core.TradeSizingModePercent, TradeScaleFactor: 0.10, TradeSizeUSDC: 10.0, MinMarginPercent: 1.0, BinanceSignalThresholdPct: 0.05, PaperBinanceExecutionDelayMs: 250, PaperArbMode: "taker", CopytradePollIntervalMs: 2000, CopytradeSizingMode: core.CopytradeSizingModeUSDC, CopytradeSizeUSDC: 10.0, CopytradeSizeShares: 10.0, CopytradeSizePercent: 100.0, CopytradeMaxSlippagePct: 1.0, LadderedTakerSizingMode: core.LadderedTakerSizingModeUSDC, LadderedTakerSizeUSDC: 10.0, LadderedTakerSizeShares: 10.0, LadderedTakerReentryMoveCents: 1.0, LadderedTakerMaxSlippagePct: 1.0, LadderedTakerPnLGuardMode: core.LadderedTakerPnLGuardWorst, LadderedTakerWorstPnLFloor: 0, LadderedTakerMaxProfitPnL: 0, BuyExecutionMarginFloorPercent: -0.01, SplitMinMarginSell: 2.0, MakerMergeBufferSeconds: 30, MakerQuoteGap: 0.008, MakerInventoryTargetMult: 3.0, MakerInventoryCapMult: 5.0, MakerMinQuoteValue: 5.0, MinAskPrice: 0.10, MaxAskPrice: 0.90, TradingHoursMode: "weekdays trade only", TakerCloseMarket: false, TakerCloseMarketTime: 5, TakerCloseMarketSlippage: 0.99, TakerCloseMarketMinPrice: 0.60, TakerCloseSizingMode: core.TakerCloseSizingModePercent, TakerCloseSizeUSDC: 1.0, TakerCloseSizeShares: 1.02}
)

const (
	settingsRowMarket = iota
	settingsRowMaxMarkets
	settingsRowPaperBalance
	settingsRowTimeframe
	settingsRowTradeSizingMode
	settingsRowTradeSizingValue
	settingsRowLadderCooldown
	settingsRowLadderSlippage
	settingsRowLadderPnLGuardMode
	settingsRowLadderWorstPnLFloor
	settingsRowMinMargin
	settingsRowBinanceExecutionDelay
	settingsRowPaperArbMode
	settingsRowCopytradeTarget
	settingsRowCopytradePoll
	settingsRowExecutionSlip
	settingsRowSplitMinMargin
	settingsRowSplitStrategy
	settingsRowSplitInitialCap
	settingsRowSplitReplenishCap
	settingsRowTakerCloseMarket
	settingsRowBlockPendingRedemption
	settingsRowOneHourCryptoExit
	settingsRowRedeemEntryTiming
	settingsRowRedeemGasMode
	settingsRowMinAskPrice
	settingsRowMaxAskPrice
	settingsRowMakerMergeBuffer
	settingsRowMakerQuoteGap
	settingsRowMakerTargetMult
	settingsRowMakerCapMult
	settingsRowMakerMinQuoteValue
	settingsRowMaxTradeSize
	settingsRowMaxDailyLoss
	settingsRowExchange
	settingsRowExecutionBackend
	settingsRowTakerCloseTime
	settingsRowTakerCloseSlippage
	settingsRowTakerCloseMinPrice
	settingsRowTradingHoursMode
	settingsRowRPCEdit
	settingsRowPrivateKeyEdit
	settingsRowCount
)

func (m tuiModel) toggleExchange() (tea.Model, tea.Cmd) {
	if m.tui.settings.Exchange == "polymarket" {
		m.tui.settings.Exchange = "kalshi"
		// Kalshi websockets require an API key even for market data.
		if os.Getenv("KALSHI_API_KEY") == "" {
			m.tui.eventLog = append(m.tui.eventLog, "⚠️ Kalshi keys missing. Please restart the app to configure.")
			if len(m.tui.eventLog) > m.tui.maxEvents {
				m.tui.eventLog = m.tui.eventLog[len(m.tui.eventLog)-m.tui.maxEvents:]
			}
		}
	} else {
		m.tui.settings.Exchange = "polymarket"
	}
	return m, nil
}

func isMakerSettingsMode(cfg TUISettings) bool {
	return strings.EqualFold(cfg.PaperArbMode, "maker")
}

func isCopytradeSettingsMode(cfg TUISettings) bool {
	return strings.EqualFold(cfg.PaperArbMode, "copytrade")
}

func isLadderedTakerSettingsMode(cfg TUISettings) bool {
	return strings.EqualFold(cfg.PaperArbMode, "laddered-taker")
}

func isLadderedTakerShareSizingMode(cfg TUISettings) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.LadderedTakerSizingMode), core.LadderedTakerSizingModeShares)
}

func normalizedTakerCloseSizingMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case core.TakerCloseSizingModeUSDC:
		return core.TakerCloseSizingModeUSDC
	case core.TakerCloseSizingModeShares:
		return core.TakerCloseSizingModeShares
	default:
		return core.TakerCloseSizingModePercent
	}
}

func isTakerCloseShareSizingMode(cfg TUISettings) bool {
	return strings.EqualFold(normalizedTakerCloseSizingMode(cfg.TakerCloseSizingMode), core.TakerCloseSizingModeShares)
}

func isTakerCloseUSDCSizingMode(cfg TUISettings) bool {
	return strings.EqualFold(normalizedTakerCloseSizingMode(cfg.TakerCloseSizingMode), core.TakerCloseSizingModeUSDC)
}

func isLadderedTakerWorstPnLMode(cfg TUISettings) bool {
	return !strings.EqualFold(strings.TrimSpace(cfg.LadderedTakerPnLGuardMode), core.LadderedTakerPnLGuardMaxProfit)
}

func isBinanceGapSettingsMode(cfg TUISettings) bool {
	return strings.EqualFold(cfg.PaperArbMode, "binance-gap")
}

func TakerCloseModeActive(cfg TUISettings) bool {
	return cfg.TakerCloseMarket && !isMakerSettingsMode(cfg) && !isCopytradeSettingsMode(cfg) && !isBinanceGapSettingsMode(cfg) && !isLadderedTakerSettingsMode(cfg)
}

func realbotPaperBackendDisablesMaker(cfg TUISettings, mode string) bool {
	return strings.EqualFold(mode, "Real") && strings.EqualFold(cfg.ExecutionBackend, core.ExecutionBackendPaper)
}

func realbotPaperBackendDisablesSplit(cfg TUISettings, mode string) bool {
	return strings.EqualFold(mode, "Real") && strings.EqualFold(cfg.ExecutionBackend, core.ExecutionBackendPaper)
}

func usesPaperExecutionSemantics(cfg TUISettings, mode string) bool {
	return strings.EqualFold(mode, "Paper") || realbotPaperBackendDisablesMaker(cfg, mode)
}

func settingsArbModes(cfg TUISettings, mode string) []string {
	modes := []string{"taker", "laddered-taker", "binance-gap", "copytrade"}
	if !realbotPaperBackendDisablesMaker(cfg, mode) {
		modes = append(modes, "maker")
	}
	return modes
}

func normalizeTUISettingsForContext(s TUISettings, mode string) TUISettings {
	s = normalizeTUISettings(s)
	if realbotPaperBackendDisablesMaker(s, mode) && strings.EqualFold(s.PaperArbMode, "maker") {
		s.PaperArbMode = "taker"
	}
	if realbotPaperBackendDisablesSplit(s, mode) {
		s.SplitStrategyEnabled = false
	}
	return s
}

func isRowVisible(cfg TUISettings, mode string, idx int) bool {
	maker := isMakerSettingsMode(cfg)
	copytrade := isCopytradeSettingsMode(cfg)
	laddered := isLadderedTakerSettingsMode(cfg)
	binanceGap := isBinanceGapSettingsMode(cfg)
	kalshi := cfg.Exchange == "kalshi"
	closeMarket := TakerCloseModeActive(cfg)
	paperMode := strings.EqualFold(mode, "Paper") || strings.EqualFold(cfg.ExecutionBackend, core.ExecutionBackendPaper)

	if idx == settingsRowPaperBalance {
		return paperMode
	}
	if idx == settingsRowExecutionBackend {
		return !strings.EqualFold(mode, "Paper")
	}
	if idx == settingsRowRedeemEntryTiming {
		return laddered && cfg.BlockNewEntriesOnPendingRedemption
	}
	if idx == settingsRowOneHourCryptoExit {
		return laddered && !kalshi && normalizeMarketTimeframe(cfg.Timeframe) == "1h"
	}
	if idx == settingsRowRedeemGasMode {
		return strings.EqualFold(mode, "Real") &&
			!strings.EqualFold(cfg.ExecutionBackend, core.ExecutionBackendPaper) &&
			!kalshi
	}

	if kalshi {
		// Kalshi uses its own scheduling and does not support split inventory.
		switch idx {
		case settingsRowTimeframe, settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap, settingsRowCopytradeTarget, settingsRowCopytradePoll:
			return false
		}
	}

	if copytrade {
		switch idx {
		case settingsRowMinMargin, settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap, settingsRowTakerCloseMarket, settingsRowMinAskPrice, settingsRowMaxAskPrice, settingsRowMakerMergeBuffer, settingsRowMakerQuoteGap, settingsRowMakerTargetMult, settingsRowMakerCapMult, settingsRowMakerMinQuoteValue, settingsRowTakerCloseTime, settingsRowTakerCloseSlippage, settingsRowTakerCloseMinPrice:
			return false
		}
	}

	if laddered {
		switch idx {
		case settingsRowMinMargin, settingsRowExecutionSlip, settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap, settingsRowTakerCloseMarket:
			return false
		}
	}

	if binanceGap {
		switch idx {
		case settingsRowExecutionSlip, settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap, settingsRowTakerCloseMarket, settingsRowTakerCloseTime, settingsRowTakerCloseSlippage, settingsRowTakerCloseMinPrice, settingsRowCopytradeTarget, settingsRowCopytradePoll:
			return false
		}
	}

	if closeMarket && !maker {
		// Taker-close mode bypasses the normal split/panic-buy paths, so hide
		// controls that do not affect the dedicated close-market execution flow.
		switch idx {
		case settingsRowMinMargin, settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap, settingsRowMinAskPrice, settingsRowMaxAskPrice:
			return false
		}
	}

	switch idx {
	case settingsRowLadderCooldown:
		return laddered
	case settingsRowLadderSlippage, settingsRowLadderPnLGuardMode, settingsRowLadderWorstPnLFloor:
		return laddered
	case settingsRowCopytradeTarget, settingsRowCopytradePoll:
		return copytrade
	case settingsRowBinanceExecutionDelay:
		return binanceGap && paperMode
	case settingsRowExecutionSlip:
		return !maker && !binanceGap
	case settingsRowSplitMinMargin, settingsRowSplitStrategy, settingsRowSplitInitialCap, settingsRowSplitReplenishCap:
		if realbotPaperBackendDisablesSplit(cfg, mode) {
			return false
		}
		return !maker && !binanceGap && !copytrade && !laddered
	case settingsRowMakerMergeBuffer, settingsRowMakerQuoteGap, settingsRowMakerTargetMult, settingsRowMakerCapMult, settingsRowMakerMinQuoteValue:
		return maker
	case settingsRowTakerCloseTime, settingsRowTakerCloseSlippage, settingsRowTakerCloseMinPrice:
		return closeMarket && !copytrade && !binanceGap
	default:
		return true
	}
}

func isStructuralSetting(idx int) bool {
	switch idx {
	case settingsRowMarket,
		settingsRowMaxMarkets,
		settingsRowTimeframe,
		settingsRowPaperArbMode,
		settingsRowExchange,
		settingsRowExecutionBackend:
		return true
	default:
		return false
	}
}

func settingsRequireRestart(prev, next TUISettings) bool {
	return !strings.EqualFold(strings.TrimSpace(prev.MarketSlug), strings.TrimSpace(next.MarketSlug)) ||
		prev.MaxMarkets != next.MaxMarkets ||
		!strings.EqualFold(normalizeMarketTimeframe(prev.Timeframe), normalizeMarketTimeframe(next.Timeframe)) ||
		!strings.EqualFold(strings.TrimSpace(prev.PaperArbMode), strings.TrimSpace(next.PaperArbMode)) ||
		!strings.EqualFold(strings.TrimSpace(prev.Exchange), strings.TrimSpace(next.Exchange)) ||
		!strings.EqualFold(strings.TrimSpace(prev.ExecutionBackend), strings.TrimSpace(next.ExecutionBackend))
}

func settingsRowEditable(cfg TUISettings, mode string, idx int) bool {
	return isRowVisible(cfg, mode, idx)
}

func ensureVisibleSettingsCursor(cfg TUISettings, mode string, cursor int) int {
	if settingsRowCount <= 0 {
		return 0
	}
	if cursor < 0 {
		cursor = 0
	}
	cursor = cursor % settingsRowCount
	if isRowVisible(cfg, mode, cursor) {
		return cursor
	}
	for i := 1; i < settingsRowCount; i++ {
		idx := (cursor + i) % settingsRowCount
		if isRowVisible(cfg, mode, idx) {
			return idx
		}
	}
	return 0
}

func settingsRowLabel(cfg TUISettings, idx int) string {
	maker := isMakerSettingsMode(cfg)
	copytrade := isCopytradeSettingsMode(cfg)
	laddered := isLadderedTakerSettingsMode(cfg)
	binanceGap := isBinanceGapSettingsMode(cfg)
	switch idx {
	case settingsRowPaperBalance:
		return "Paper Balance"
	case settingsRowExecutionBackend:
		return "Execution Backend"
	case settingsRowTradeSizingMode:
		if TakerCloseModeActive(cfg) {
			return "Close Size Mode"
		}
		if copytrade {
			return "Copy Size Mode"
		}
		if laddered {
			return "Ladder Size Mode"
		}
		return "Trade Size Mode"
	case settingsRowTradeSizingValue:
		if TakerCloseModeActive(cfg) {
			if isTakerCloseShareSizingMode(cfg) {
				return "Close Size (Shares)"
			}
			if isTakerCloseUSDCSizingMode(cfg) {
				return "Close Size (USDC)"
			}
			return "Close Size (%)"
		}
		if copytrade {
			if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModeShares) {
				return "Copy Size (Shares)"
			}
			if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModePercent) {
				return "Copy Size (% Master)"
			}
			return "Copy Size (USDC)"
		}
		if laddered {
			if isLadderedTakerShareSizingMode(cfg) {
				return "Ladder Size (Shares)"
			}
			return "Ladder Size (USDC)"
		}
		if strings.EqualFold(cfg.TradeSizingMode, core.TradeSizingModeUSDC) {
			return "Trade Size (USDC)"
		}
		return "Trade Scale Factor"
	case settingsRowLadderCooldown:
		return "Ladder Re-entry Move"
	case settingsRowLadderSlippage:
		return "Ladder Max Slippage %"
	case settingsRowLadderPnLGuardMode:
		return "Ladder PnL Guard"
	case settingsRowLadderWorstPnLFloor:
		if isLadderedTakerWorstPnLMode(cfg) {
			return "Ladder Worst PnL Floor"
		}
		return "Ladder Min Profit PnL"
	case settingsRowMinMargin:
		if maker {
			return "Maker Min Sell Edge %"
		}
		if copytrade {
			return "Copy Margin"
		}
		if laddered {
			return "Ladder Min Margin %"
		}
		if binanceGap {
			return "Profit Target %"
		}
		return "Buy Min Margin %"
	case settingsRowBinanceExecutionDelay:
		return "Paper Exec Delay"
	case settingsRowExecutionSlip:
		if copytrade {
			return "Copy Max Slip"
		}
		return "Max Exec Slip %"
	case settingsRowCopytradeTarget:
		return "Copytrade Target"
	case settingsRowCopytradePoll:
		return "Copytrade Poll"
	case settingsRowSplitMinMargin:
		return "Split Min Margin"
	case settingsRowSplitStrategy:
		return "Split Strategy"
	case settingsRowSplitInitialCap:
		return "Split Initial Cap"
	case settingsRowSplitReplenishCap:
		return "Split Replenish Cap"
	case settingsRowTakerCloseMarket:
		return "Taker Close Market"
	case settingsRowBlockPendingRedemption:
		return "Wait Redeem Before Entry"
	case settingsRowOneHourCryptoExit:
		return "1h Crypto Exit"
	case settingsRowRedeemEntryTiming:
		return "Redeem Re-entry Timing"
	case settingsRowRedeemGasMode:
		return "Redeem Gas Speed"
	case settingsRowMinAskPrice:
		if maker {
			return "Maker Min Buy Price"
		}
		return "Min Ask Price"
	case settingsRowMaxAskPrice:
		if maker {
			return "Maker Max Buy Price"
		}
		return "Max Ask Price"
	case settingsRowMakerMergeBuffer:
		return "Maker Merge Buffer"
	case settingsRowMakerQuoteGap:
		return "Maker Quote Gap"
	case settingsRowMakerTargetMult:
		return "Maker Target Mult"
	case settingsRowMakerCapMult:
		return "Maker Cap Mult"
	case settingsRowMakerMinQuoteValue:
		return "Maker Min Quote ($)"
	case settingsRowMaxTradeSize:
		return "Max Trade Size"
	case settingsRowMaxDailyLoss:
		return "Max Daily Loss"
	case settingsRowExchange:
		return "Exchange"
	case settingsRowTakerCloseTime:
		return "Taker Close Time"
	case settingsRowTakerCloseSlippage:
		return "Taker Close Slippage"
	case settingsRowTakerCloseMinPrice:
		return "Taker Close Min Price"
	case settingsRowTradingHoursMode:
		return "Trading Hours (WIB)"
	case settingsRowRPCEdit:
		return "RPC URL (Press Enter to edit)"
	case settingsRowPrivateKeyEdit:
		return "Private Key (Press Enter to edit)"
	default:
		return ""
	}
}

func normalizeMarketSelection(slug string) string {
	slug = strings.TrimSpace(slug)
	if slug == "" || strings.EqualFold(slug, "ALL") {
		return "ALL"
	}
	parts := strings.Split(slug, ",")
	seen := make(map[string]bool, len(parts))
	normalized := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ToUpper(strings.TrimSpace(part))
		if part == "" || part == "ALL" || seen[part] {
			continue
		}
		seen[part] = true
		normalized = append(normalized, part)
	}
	if len(normalized) == 0 {
		return "ALL"
	}
	return strings.Join(normalized, ",")
}

func normalizeMarketTimeframe(timeframe string) string {
	switch strings.ToLower(strings.TrimSpace(timeframe)) {
	case "5m":
		return "5m"
	case "1h":
		return "1h"
	case "4h":
		return "4h"
	case "1d":
		return "1d"
	default:
		return "15m"
	}
}

func cycleMarketTimeframe(current string, delta int) string {
	return cycleString([]string{"15m", "5m", "1h", "4h", "1d"}, normalizeMarketTimeframe(current), delta)
}

func normalizeTUISettings(s TUISettings) TUISettings {
	s.MarketSlug = normalizeMarketSelection(s.MarketSlug)
	s.Timeframe = normalizeMarketTimeframe(s.Timeframe)
	if mode, ok := core.NormalizeTradingHoursMode(s.TradingHoursMode); ok {
		s.TradingHoursMode = mode
	} else {
		s.TradingHoursMode = core.TradingHoursModeOff
	}
	if strings.EqualFold(strings.TrimSpace(s.ExecutionBackend), core.ExecutionBackendLive) {
		s.ExecutionBackend = core.ExecutionBackendLive
	} else {
		s.ExecutionBackend = core.ExecutionBackendPaper
	}
	if s.PaperBalance <= 0 {
		s.PaperBalance = 100.0
	}
	s.PaperBalance = math.Round(s.PaperBalance*100.0) / 100.0
	switch strings.ToLower(strings.TrimSpace(s.PaperArbMode)) {
	case "maker":
		s.PaperArbMode = "maker"
	case "copytrade":
		s.PaperArbMode = "copytrade"
	case "laddered-taker":
		s.PaperArbMode = "laddered-taker"
	case "binance-gap":
		s.PaperArbMode = "binance-gap"
	default:
		s.PaperArbMode = "taker"
	}
	if strings.EqualFold(strings.TrimSpace(s.TradeSizingMode), core.TradeSizingModeUSDC) {
		s.TradeSizingMode = core.TradeSizingModeUSDC
	} else {
		s.TradeSizingMode = core.TradeSizingModePercent
	}
	s.TradeSizeUSDC = normalizeTUIFixedTradeSizeUSDC(s.TradeSizeUSDC, math.Max(s.TradeScaleFactor*100.0, 1.0))
	if s.TradeScaleFactor <= 0 {
		s.TradeScaleFactor = 0.01
	}
	if s.TradeScaleFactor > 1.0 {
		s.TradeScaleFactor = 1.0
	}
	if s.MaxMarkets < 1 {
		s.MaxMarkets = 1
	}
	if s.MarketSlug != "ALL" {
		selected := len(strings.Split(s.MarketSlug, ","))
		if selected > 0 && s.MaxMarkets > selected {
			s.MaxMarkets = selected
		}
	}
	s.BuyExecutionMarginFloorPercent = normalizeExecutionFloorSetting(s.BuyExecutionMarginFloorPercent)
	if s.CopytradeMaxSlippagePct > 99.0 {
		s.CopytradeMaxSlippagePct = 99.0
	}
	if s.CopytradeMaxSlippagePct < 0 {
		s.CopytradeMaxSlippagePct = 0
	}
	s.CopytradeMaxSlippagePct = math.Round(s.CopytradeMaxSlippagePct)
	s.TakerCloseMarketSlippage = normalizeTakerClosePriceSetting(s.TakerCloseMarketSlippage, 0.99)
	s.TakerCloseMarketMinPrice = normalizeTakerClosePriceSetting(s.TakerCloseMarketMinPrice, 0.60)
	if s.TakerCloseMarketTime < 0 {
		s.TakerCloseMarketTime = 0
	}
	if s.TakerCloseMarketTime > 60 {
		s.TakerCloseMarketTime = 60
	}
	if strings.TrimSpace(s.TakerCloseSizingMode) == "" {
		s.TakerCloseSizingMode = normalizedTakerCloseSizingMode(s.TradeSizingMode)
	} else {
		s.TakerCloseSizingMode = normalizedTakerCloseSizingMode(s.TakerCloseSizingMode)
	}
	s.TakerCloseSizeUSDC = normalizeTUIFixedTradeSizeUSDC(s.TakerCloseSizeUSDC, math.Max(s.TradeSizeUSDC, 1.0))
	if s.TakerCloseSizeShares <= 0 {
		s.TakerCloseSizeShares = 1.02
	}
	s.TakerCloseSizeShares = math.Round(s.TakerCloseSizeShares*100.0) / 100.0
	if s.TakerCloseSizeShares < 1.02 {
		s.TakerCloseSizeShares = 1.02
	}
	s.CopytradeTarget = strings.TrimSpace(s.CopytradeTarget)
	if s.CopytradePollIntervalMs <= 0 {
		s.CopytradePollIntervalMs = 2000
	}
	if s.CopytradePollIntervalMs < 100 {
		s.CopytradePollIntervalMs = 100
	}
	if s.CopytradePollIntervalMs > 30000 {
		s.CopytradePollIntervalMs = 30000
	}
	switch strings.ToLower(strings.TrimSpace(s.CopytradeSizingMode)) {
	case core.CopytradeSizingModeShares:
		s.CopytradeSizingMode = core.CopytradeSizingModeShares
	case core.CopytradeSizingModePercent:
		s.CopytradeSizingMode = core.CopytradeSizingModePercent
	default:
		s.CopytradeSizingMode = core.CopytradeSizingModeUSDC
	}
	s.CopytradeSizeUSDC = normalizeTUIFixedTradeSizeUSDC(s.CopytradeSizeUSDC, math.Max(s.TradeSizeUSDC, 1.0))
	if s.CopytradeSizeShares <= 0 {
		s.CopytradeSizeShares = 1.0
	}
	s.CopytradeSizeShares = math.Round(s.CopytradeSizeShares*100.0) / 100.0
	if s.CopytradeSizeShares < 0.01 {
		s.CopytradeSizeShares = 0.01
	}
	if s.CopytradeSizePercent <= 0 {
		s.CopytradeSizePercent = 100.0
	}
	s.CopytradeSizePercent = math.Round(s.CopytradeSizePercent*10.0) / 10.0
	if s.CopytradeSizePercent < 0.1 {
		s.CopytradeSizePercent = 0.1
	}
	if s.CopytradeSizePercent > 100.0 {
		s.CopytradeSizePercent = 100.0
	}
	switch strings.ToLower(strings.TrimSpace(s.LadderedTakerSizingMode)) {
	case core.LadderedTakerSizingModeShares:
		s.LadderedTakerSizingMode = core.LadderedTakerSizingModeShares
	default:
		s.LadderedTakerSizingMode = core.LadderedTakerSizingModeUSDC
	}
	switch strings.ToLower(strings.TrimSpace(s.LadderedTakerPnLGuardMode)) {
	case core.LadderedTakerPnLGuardMaxProfit:
		s.LadderedTakerPnLGuardMode = core.LadderedTakerPnLGuardMaxProfit
	default:
		s.LadderedTakerPnLGuardMode = core.LadderedTakerPnLGuardWorst
	}
	s.LadderedTakerSizeUSDC = normalizeTUIFixedTradeSizeUSDC(s.LadderedTakerSizeUSDC, math.Max(s.TradeSizeUSDC, 1.0))
	if s.LadderedTakerSizeShares <= 0 {
		s.LadderedTakerSizeShares = 1.0
	}
	s.LadderedTakerSizeShares = math.Round(s.LadderedTakerSizeShares*100.0) / 100.0
	if s.LadderedTakerSizeShares < 0.01 {
		s.LadderedTakerSizeShares = 0.01
	}
	s.LadderedTakerReentryMoveCents = normalizeLadderedTakerReentryMoveCents(s.LadderedTakerReentryMoveCents)
	s.RedeemEntryTiming = normalizeRedeemEntryTiming(s.RedeemEntryTiming)
	s.RedeemGasMode = normalizeRedeemGasMode(s.RedeemGasMode)
	s.OneHourCryptoExitMode = normalizeOneHourCryptoExitMode(s.OneHourCryptoExitMode)
	if s.LadderedTakerMaxSlippagePct < 0 {
		s.LadderedTakerMaxSlippagePct = 0
	} else if s.LadderedTakerMaxSlippagePct > 99.0 {
		s.LadderedTakerMaxSlippagePct = 99.0
	}
	s.LadderedTakerMaxSlippagePct = math.Round(s.LadderedTakerMaxSlippagePct)
	s.LadderedTakerWorstPnLFloor = normalizeLadderedTakerWorstPnLFloor(s.LadderedTakerWorstPnLFloor)
	s.LadderedTakerMaxProfitPnL = normalizeLadderedTakerMaxProfitPnL(s.LadderedTakerMaxProfitPnL)
	if s.BinanceSignalThresholdPct <= 0 {
		s.BinanceSignalThresholdPct = 0.02
	}
	s.BinanceSignalThresholdPct = math.Round(s.BinanceSignalThresholdPct*1000.0) / 1000.0
	if s.BinanceSignalThresholdPct < 0.005 {
		s.BinanceSignalThresholdPct = 0.005
	}
	if s.BinanceSignalThresholdPct > 5.0 {
		s.BinanceSignalThresholdPct = 5.0
	}
	s.PaperBinanceExecutionDelayMs = int(math.Round(float64(s.PaperBinanceExecutionDelayMs)/10.0) * 10.0)
	if s.PaperBinanceExecutionDelayMs < 0 {
		s.PaperBinanceExecutionDelayMs = 0
	}
	if s.PaperBinanceExecutionDelayMs > 5000 {
		s.PaperBinanceExecutionDelayMs = 5000
	}
	if s.TakerCloseMarketSlippage < s.TakerCloseMarketMinPrice {
		s.TakerCloseMarketSlippage = s.TakerCloseMarketMinPrice
	}
	return s
}

func cycleCopytradeSizingMode(mode string, delta int) string {
	modes := []string{
		core.CopytradeSizingModeUSDC,
		core.CopytradeSizingModeShares,
		core.CopytradeSizingModePercent,
	}
	current := normalizeTUISettings(TUISettings{CopytradeSizingMode: mode}).CopytradeSizingMode
	idx := 0
	for i, candidate := range modes {
		if current == candidate {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(modes)) % len(modes)
	return modes[idx]
}

func cycleTakerCloseSizingMode(mode string, delta int) string {
	modes := []string{
		core.TakerCloseSizingModePercent,
		core.TakerCloseSizingModeUSDC,
		core.TakerCloseSizingModeShares,
	}
	current := normalizedTakerCloseSizingMode(mode)
	idx := 0
	for i, candidate := range modes {
		if current == candidate {
			idx = i
			break
		}
	}
	idx = (idx + delta) % len(modes)
	if idx < 0 {
		idx += len(modes)
	}
	return modes[idx]
}

func cycleLadderedTakerSizingMode(mode string, delta int) string {
	modes := []string{
		core.LadderedTakerSizingModeUSDC,
		core.LadderedTakerSizingModeShares,
	}
	current := normalizeTUISettings(TUISettings{LadderedTakerSizingMode: mode}).LadderedTakerSizingMode
	idx := 0
	for i, candidate := range modes {
		if current == candidate {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(modes)) % len(modes)
	return modes[idx]
}

func cycleLadderedTakerPnLGuardMode(mode string, delta int) string {
	modes := []string{
		core.LadderedTakerPnLGuardWorst,
		core.LadderedTakerPnLGuardMaxProfit,
	}
	current := normalizeTUISettings(TUISettings{LadderedTakerPnLGuardMode: mode}).LadderedTakerPnLGuardMode
	idx := 0
	for i, candidate := range modes {
		if current == candidate {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(modes)) % len(modes)
	return modes[idx]
}

func normalizeRedeemEntryTiming(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case core.RedeemEntryTimingImmediate:
		return core.RedeemEntryTimingImmediate
	default:
		return core.RedeemEntryTimingNextMarket
	}
}

func normalizeRedeemGasMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case core.RedeemGasModeNormal:
		return core.RedeemGasModeNormal
	case core.RedeemGasModeUrgent:
		return core.RedeemGasModeUrgent
	default:
		return core.RedeemGasModeFast
	}
}

func normalizeOneHourCryptoExitMode(mode string) string {
	return core.NormalizeOneHourCryptoExitMode(mode)
}

func cycleRedeemEntryTiming(mode string, delta int) string {
	modes := []string{
		core.RedeemEntryTimingNextMarket,
		core.RedeemEntryTimingImmediate,
	}
	current := normalizeRedeemEntryTiming(mode)
	idx := 0
	for i, candidate := range modes {
		if current == candidate {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(modes)) % len(modes)
	return modes[idx]
}

func cycleOneHourCryptoExitMode(mode string, delta int) string {
	modes := []string{
		core.OneHourCryptoExitSell999,
		core.OneHourCryptoExitWaitResolve,
	}
	current := normalizeOneHourCryptoExitMode(mode)
	idx := 0
	for i, candidate := range modes {
		if current == candidate {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(modes)) % len(modes)
	return modes[idx]
}

func cycleRedeemGasMode(mode string, delta int) string {
	modes := []string{
		core.RedeemGasModeNormal,
		core.RedeemGasModeFast,
		core.RedeemGasModeUrgent,
	}
	current := normalizeRedeemGasMode(mode)
	idx := 0
	for i, candidate := range modes {
		if current == candidate {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(modes)) % len(modes)
	return modes[idx]
}

func normalizeTakerClosePriceSetting(v, fallback float64) float64 {
	if v <= 0 || v >= 1.0 {
		v = fallback
	}
	v = math.Round(v*100.0) / 100.0
	if v < 0.01 {
		return 0.01
	}
	if v > 0.99 {
		return 0.99
	}
	return v
}

func normalizeLadderedTakerReentryMoveCents(v float64) float64 {
	if v <= 0 {
		return 1.0
	}
	v = math.Round(v*10.0) / 10.0
	if v < 1.0 {
		return 1.0
	}
	if v > 25.0 {
		return 25.0
	}
	return v
}

func normalizeLadderedTakerWorstPnLFloor(v float64) float64 {
	switch {
	case math.IsNaN(v), math.IsInf(v, 0):
		return 0
	}
	v = math.Round(v*100.0) / 100.0
	if v > 0 {
		v = -v
	}
	if math.Abs(v) < 0.005 {
		return 0
	}
	if v < -1000.0 {
		return -1000.0
	}
	return v
}

func normalizeLadderedTakerMaxProfitPnL(v float64) float64 {
	switch {
	case math.IsNaN(v), math.IsInf(v, 0):
		return 0
	}
	v = math.Round(v*100.0) / 100.0
	if math.Abs(v) < 0.005 {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1000.0 {
		return 1000.0
	}
	return v
}

func normalizeExecutionFloorSetting(v float64) float64 {
	// Support both legacy percent form (-1.0 == -1%) and decimal form
	// (-0.01 == -1%), but keep the runtime/UI value in decimal slippage form.
	if math.Abs(v) > 0.10 {
		v = v / 100.0
	}
	if v > 0 {
		v = 0
	}
	if v < -0.10 {
		v = -0.10
	}
	return v
}

func executionFloorDisplayPercent(v float64) float64 {
	return normalizeExecutionFloorSetting(v) * 100.0
}

func normalizeCopytradeTargetInput(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return raw
}

func renderCopytradeTargetValue(raw string, editing bool, buffer string) string {
	target := normalizeCopytradeTargetInput(raw)
	if editing {
		value := normalizeCopytradeTargetInput(buffer)
		if value == "" {
			value = "paste wallet / @handle / profile URL"
		}
		return styleCyan.Render(" " + value + " _ ")
	}
	if target == "" {
		return styleMuted.Render(" paste target ")
	}
	if len(target) > 28 {
		target = target[:25] + "..."
	}
	return styleCyan.Render(" " + target + " ")
}

func renderStringValue(raw string, editing bool, buffer string, placeholder string) string {
	target := strings.TrimSpace(raw)
	if editing {
		value := strings.TrimSpace(buffer)
		if value == "" {
			value = placeholder
		}
		return styleCyan.Render(" " + value + " _ ")
	}
	if target == "" {
		return styleMuted.Render(" " + placeholder + " ")
	}
	if len(target) > 28 {
		target = target[:25] + "..."
	}
	return styleCyan.Render(" " + target + " ")
}

func fmtFloatTrim(v float64, decimals int) string {
	s := strconv.FormatFloat(v, 'f', decimals, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" {
		return "0"
	}
	return s
}

func formatDisplayShareQty(v float64) string {
	if math.Abs(v-math.Round(v)) < 1e-9 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmtFloatTrim(v, 5)
}

func formatSignedDisplayShareQty(v float64) string {
	switch {
	case v > 0:
		return "+" + formatDisplayShareQty(v)
	case v < 0:
		return "-" + formatDisplayShareQty(math.Abs(v))
	default:
		return "0"
	}
}

func displayedTradeBudgetsWithMode(mode string, cash, equity, startingBalance, sizingBalance, tradeFactor, tradeSizeUSDC, maxTradeSize, multiplier float64, tradeSizingMode string) (base, effective float64) {
	sizingCapital := equity
	if strings.EqualFold(mode, "Real") || strings.EqualFold(mode, "Live") {
		sizingCapital = equity
		if sizingCapital <= 0 {
			sizingCapital = math.Max(cash, startingBalance)
		}
		if cash > sizingCapital {
			sizingCapital = cash
		}
	}

	base = core.CalculateTradeSizeForMode(sizingCapital, tradeFactor, tradeSizeUSDC, maxTradeSize, tradeSizingMode)
	if base <= 0 {
		return 0, 0
	}

	effective = base
	if strings.EqualFold(mode, "Paper") && multiplier > 1.0 && !strings.EqualFold(tradeSizingMode, core.TradeSizingModeUSDC) {
		effective = base * multiplier
	}
	return base, effective
}

func settingsEditValue(cfg TUISettings, row int) string {
	switch row {
	case settingsRowCopytradeTarget:
		return cfg.CopytradeTarget
	case settingsRowTradingHoursMode:
		mode := strings.ToLower(strings.TrimSpace(cfg.TradingHoursMode))
		if mode == "off" || mode == "weekdays trade only" || mode == "us open only" {
			return "08:00-17:00"
		}
		return cfg.TradingHoursMode
	case settingsRowPaperBalance:
		return fmt.Sprintf("%.2f", cfg.PaperBalance)
	case settingsRowTradeSizingValue:
		if TakerCloseModeActive(cfg) {
			if isTakerCloseShareSizingMode(cfg) {
				return fmt.Sprintf("%.2f", cfg.TakerCloseSizeShares)
			}
			if isTakerCloseUSDCSizingMode(cfg) {
				return fmt.Sprintf("%.2f", cfg.TakerCloseSizeUSDC)
			}
			return fmt.Sprintf("%.3f", cfg.TradeScaleFactor)
		}
		if isCopytradeSettingsMode(cfg) {
			if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModeShares) {
				return fmt.Sprintf("%.2f", cfg.CopytradeSizeShares)
			}
			if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModePercent) {
				return fmt.Sprintf("%.2f", cfg.CopytradeSizePercent)
			}
			return fmt.Sprintf("%.2f", cfg.CopytradeSizeUSDC)
		}
		if isLadderedTakerSettingsMode(cfg) {
			if isLadderedTakerShareSizingMode(cfg) {
				return fmt.Sprintf("%.2f", cfg.LadderedTakerSizeShares)
			}
			return fmt.Sprintf("%.2f", cfg.LadderedTakerSizeUSDC)
		}
		if strings.EqualFold(cfg.TradeSizingMode, core.TradeSizingModeUSDC) {
			return fmt.Sprintf("%.2f", cfg.TradeSizeUSDC)
		}
		return fmt.Sprintf("%.3f", cfg.TradeScaleFactor)
	case settingsRowExecutionSlip:
		if isCopytradeSettingsMode(cfg) {
			return fmt.Sprintf("%.0f", cfg.CopytradeMaxSlippagePct)
		}
		return fmt.Sprintf("%.1f", executionFloorDisplayPercent(cfg.BuyExecutionMarginFloorPercent))
	case settingsRowLadderSlippage:
		return fmt.Sprintf("%.0f", cfg.LadderedTakerMaxSlippagePct)
	case settingsRowLadderPnLGuardMode:
		return cfg.LadderedTakerPnLGuardMode
	case settingsRowLadderWorstPnLFloor:
		if isLadderedTakerWorstPnLMode(cfg) {
			if math.Abs(cfg.LadderedTakerWorstPnLFloor) < 0.005 {
				return "OFF"
			}
			return fmt.Sprintf("%.2f", cfg.LadderedTakerWorstPnLFloor)
		}
		if math.Abs(cfg.LadderedTakerMaxProfitPnL) < 0.005 {
			return "OFF"
		}
		return fmt.Sprintf("%.2f", cfg.LadderedTakerMaxProfitPnL)
	case settingsRowMinAskPrice:
		return fmt.Sprintf("%.2f", cfg.MinAskPrice)
	case settingsRowMaxAskPrice:
		return fmt.Sprintf("%.2f", cfg.MaxAskPrice)
	case settingsRowTakerCloseSlippage:
		return fmt.Sprintf("%.2f", cfg.TakerCloseMarketSlippage)
	case settingsRowTakerCloseMinPrice:
		return fmt.Sprintf("%.2f", cfg.TakerCloseMarketMinPrice)
	case settingsRowRPCEdit:
		return cfg.PolygonRPC
	case settingsRowPrivateKeyEdit:
		return cfg.PolygonPrivateKey
	default:
		return ""
	}
}

func settingsRowSupportsTypedEdit(cfg TUISettings, mode string, row int) bool {
	if !settingsRowEditable(cfg, mode, row) {
		return false
	}
	switch row {
	case settingsRowCopytradeTarget,
		settingsRowTradingHoursMode,
		settingsRowPaperBalance,
		settingsRowTradeSizingValue,
		settingsRowExecutionSlip,
		settingsRowLadderSlippage,
		settingsRowLadderWorstPnLFloor,
		settingsRowMinAskPrice,
		settingsRowMaxAskPrice,
		settingsRowTakerCloseTime,
		settingsRowTakerCloseSlippage,
		settingsRowTakerCloseMinPrice,
		settingsRowRPCEdit,
		settingsRowPrivateKeyEdit,
		settingsRowLadderCooldown,
		settingsRowMinMargin,
		settingsRowBinanceExecutionDelay,
		settingsRowCopytradePoll,
		settingsRowSplitMinMargin,
		settingsRowSplitInitialCap,
		settingsRowSplitReplenishCap,
		settingsRowMakerMergeBuffer,
		settingsRowMakerQuoteGap,
		settingsRowMakerTargetMult,
		settingsRowMakerCapMult,
		settingsRowMakerMinQuoteValue,
		settingsRowMaxTradeSize,
		settingsRowMaxDailyLoss:
		return true
	default:
		return false
	}
}

func settingsRowUsesFreeformTypedInput(row int) bool {
	switch row {
	case settingsRowCopytradeTarget,
		settingsRowTradingHoursMode,
		settingsRowRPCEdit,
		settingsRowPrivateKeyEdit:
		return true
	default:
		return false
	}
}

func settingsRowAllowsNegativeNumber(cfg TUISettings, row int) bool {
	switch row {
	case settingsRowLadderWorstPnLFloor:
		return isLadderedTakerWorstPnLMode(cfg)
	case settingsRowExecutionSlip:
		return !isCopytradeSettingsMode(cfg)
	default:
		return false
	}
}

func appendSettingsNumericRune(cfg TUISettings, row int, input string, r rune) (string, bool) {
	switch {
	case r >= '0' && r <= '9':
		return input + string(r), true
	case r == '.':
		if strings.ContainsRune(input, '.') {
			return input, false
		}
		switch input {
		case "":
			return "0.", true
		case "-":
			if settingsRowAllowsNegativeNumber(cfg, row) {
				return "-0.", true
			}
			return input, false
		default:
			return input + ".", true
		}
	case r == '-':
		if !settingsRowAllowsNegativeNumber(cfg, row) || input != "" {
			return input, false
		}
		return "-", true
	default:
		return input, false
	}
}

func appendSettingsTypedInput(cfg TUISettings, row int, input string, runes []rune) string {
	if settingsRowUsesFreeformTypedInput(row) {
		return input + string(runes)
	}
	for _, r := range runes {
		next, ok := appendSettingsNumericRune(cfg, row, input, r)
		if ok {
			input = next
		}
	}
	return input
}

func settingsTypedEditSeedInput(cfg TUISettings, row int, msg tea.KeyMsg) (string, bool) {
	if settingsRowUsesFreeformTypedInput(row) || len(msg.Runes) != 1 {
		return "", false
	}
	input := appendSettingsTypedInput(cfg, row, "", msg.Runes)
	return input, input != ""
}

func normalizeTUIFixedTradeSizeUSDC(size float64, fallback float64) float64 {
	if size <= 0 {
		size = fallback
	}
	size = math.Round(size*100.0) / 100.0
	if size < 1.0 {
		return 1.0
	}
	return size
}

func applySettingsEditValue(cfg *TUISettings, row int, input string) bool {
	if cfg == nil {
		return false
	}
	input = strings.TrimSpace(input)
	switch row {
	case settingsRowCopytradeTarget:
		value := normalizeCopytradeTargetInput(input)
		if normalizeCopytradeTargetInput(cfg.CopytradeTarget) == value {
			return false
		}
		cfg.CopytradeTarget = value
		return true
	case settingsRowTradingHoursMode:
		value, ok := core.NormalizeTradingHoursMode(input)
		if !ok || cfg.TradingHoursMode == value {
			return false
		}
		cfg.TradingHoursMode = value
		return true
	case settingsRowRPCEdit:
		if cfg.PolygonRPC == input {
			return false
		}
		cfg.PolygonRPC = input
		return true
	case settingsRowPrivateKeyEdit:
		if cfg.PolygonPrivateKey == input {
			return false
		}
		cfg.PolygonPrivateKey = input
		return true
	}

	value, err := strconv.ParseFloat(input, 64)
	if err != nil {
		return false
	}

	switch row {
	case settingsRowPaperBalance:
		if value <= 0 || cfg.PaperBalance == value {
			return false
		}
		cfg.PaperBalance = value
		return true
	case settingsRowTradeSizingValue:
		if value <= 0 {
			return false
		}
		if TakerCloseModeActive(*cfg) {
			if isTakerCloseShareSizingMode(*cfg) {
				if cfg.TakerCloseSizeShares == value {
					return false
				}
				cfg.TakerCloseSizeShares = value
				return true
			}
			if isTakerCloseUSDCSizingMode(*cfg) {
				value = normalizeTUIFixedTradeSizeUSDC(value, cfg.TakerCloseSizeUSDC)
				if cfg.TakerCloseSizeUSDC == value {
					return false
				}
				cfg.TakerCloseSizeUSDC = value
				return true
			}
		}
		if isCopytradeSettingsMode(*cfg) {
			if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModeShares) {
				if cfg.CopytradeSizeShares == value {
					return false
				}
				cfg.CopytradeSizeShares = value
				return true
			}
			if strings.EqualFold(cfg.CopytradeSizingMode, core.CopytradeSizingModePercent) {
				if cfg.CopytradeSizePercent == value {
					return false
				}
				cfg.CopytradeSizePercent = value
				return true
			}
			value = normalizeTUIFixedTradeSizeUSDC(value, cfg.CopytradeSizeUSDC)
			if cfg.CopytradeSizeUSDC == value {
				return false
			}
			cfg.CopytradeSizeUSDC = value
			return true
		}
		if isLadderedTakerSettingsMode(*cfg) {
			if isLadderedTakerShareSizingMode(*cfg) {
				if cfg.LadderedTakerSizeShares == value {
					return false
				}
				cfg.LadderedTakerSizeShares = value
				return true
			}
			value = normalizeTUIFixedTradeSizeUSDC(value, cfg.LadderedTakerSizeUSDC)
			if cfg.LadderedTakerSizeUSDC == value {
				return false
			}
			cfg.LadderedTakerSizeUSDC = value
			return true
		}
		if strings.EqualFold(cfg.TradeSizingMode, core.TradeSizingModeUSDC) {
			value = normalizeTUIFixedTradeSizeUSDC(value, cfg.TradeSizeUSDC)
			if cfg.TradeSizeUSDC == value {
				return false
			}
			cfg.TradeSizeUSDC = value
			return true
		}
		if cfg.TradeScaleFactor == value {
			return false
		}
		cfg.TradeScaleFactor = value
		return true
	case settingsRowLadderCooldown:
		if cfg.LadderedTakerReentryMoveCents == value {
			return false
		}
		cfg.LadderedTakerReentryMoveCents = value
		return true
	case settingsRowLadderWorstPnLFloor:
		if isLadderedTakerWorstPnLMode(*cfg) {
			if cfg.LadderedTakerWorstPnLFloor == value {
				return false
			}
			cfg.LadderedTakerWorstPnLFloor = value
			return true
		}
		if cfg.LadderedTakerMaxProfitPnL == value {
			return false
		}
		cfg.LadderedTakerMaxProfitPnL = value
		return true
	case settingsRowMinMargin:
		if cfg.MinMarginPercent == value {
			return false
		}
		cfg.MinMarginPercent = value
		return true
	case settingsRowBinanceExecutionDelay:
		if float64(cfg.PaperBinanceExecutionDelayMs) == value {
			return false
		}
		cfg.PaperBinanceExecutionDelayMs = int(value)
		return true
	case settingsRowTakerCloseTime:
		if value < 0 || float64(cfg.TakerCloseMarketTime) == value {
			return false
		}
		cfg.TakerCloseMarketTime = int(value)
		return true
	case settingsRowCopytradePoll:
		if float64(cfg.CopytradePollIntervalMs) == value {
			return false
		}
		cfg.CopytradePollIntervalMs = int(value)
		return true
	case settingsRowSplitMinMargin:
		if cfg.SplitMinMarginSell == value {
			return false
		}
		cfg.SplitMinMarginSell = value
		return true
	case settingsRowSplitInitialCap:
		if cfg.SplitInitialCapPct == value {
			return false
		}
		cfg.SplitInitialCapPct = value
		return true
	case settingsRowSplitReplenishCap:
		if cfg.SplitReplenishCapPct == value {
			return false
		}
		cfg.SplitReplenishCapPct = value
		return true
	case settingsRowMakerMergeBuffer:
		if float64(cfg.MakerMergeBufferSeconds) == value {
			return false
		}
		cfg.MakerMergeBufferSeconds = int(value)
		return true
	case settingsRowMakerQuoteGap:
		if cfg.MakerQuoteGap == value {
			return false
		}
		cfg.MakerQuoteGap = value
		return true
	case settingsRowMakerTargetMult:
		if cfg.MakerInventoryTargetMult == value {
			return false
		}
		cfg.MakerInventoryTargetMult = value
		return true
	case settingsRowMakerCapMult:
		if cfg.MakerInventoryCapMult == value {
			return false
		}
		cfg.MakerInventoryCapMult = value
		return true
	case settingsRowMakerMinQuoteValue:
		if cfg.MakerMinQuoteValue == value {
			return false
		}
		cfg.MakerMinQuoteValue = value
		return true
	case settingsRowMaxTradeSize:
		if cfg.MaxTradeSize == value {
			return false
		}
		cfg.MaxTradeSize = value
		return true
	case settingsRowMaxDailyLoss:
		if cfg.MaxDailyLoss == value {
			return false
		}
		cfg.MaxDailyLoss = value
		return true
	case settingsRowExecutionSlip:
		if isCopytradeSettingsMode(*cfg) {
			if cfg.CopytradeMaxSlippagePct == value {
				return false
			}
			cfg.CopytradeMaxSlippagePct = value
			return true
		}
		value = value / 100.0
		if cfg.BuyExecutionMarginFloorPercent == value {
			return false
		}
		cfg.BuyExecutionMarginFloorPercent = value
		return true
	case settingsRowLadderSlippage:
		if cfg.LadderedTakerMaxSlippagePct == value {
			return false
		}
		cfg.LadderedTakerMaxSlippagePct = value
		return true
	case settingsRowLadderPnLGuardMode:
		mode := strings.ToLower(strings.TrimSpace(input))
		switch mode {
		case "", "0", "auto", "worst", "worst-pnl", "worst pnl":
			mode = core.LadderedTakerPnLGuardWorst
		case "min", "profit", "max-profit", "max-profit-pnl", "min profit", "min profit pnl":
			mode = core.LadderedTakerPnLGuardMaxProfit
		default:
			return false
		}
		if cfg.LadderedTakerPnLGuardMode == mode {
			return false
		}
		cfg.LadderedTakerPnLGuardMode = mode
		return true
	case settingsRowMinAskPrice:
		if value <= 0 || cfg.MinAskPrice == value {
			return false
		}
		cfg.MinAskPrice = value
		return true
	case settingsRowMaxAskPrice:
		if value <= 0 || cfg.MaxAskPrice == value {
			return false
		}
		cfg.MaxAskPrice = value
		return true
	case settingsRowTakerCloseSlippage:
		if value <= 0 || cfg.TakerCloseMarketSlippage == value {
			return false
		}
		cfg.TakerCloseMarketSlippage = value
		return true
	case settingsRowTakerCloseMinPrice:
		if value <= 0 || cfg.TakerCloseMarketMinPrice == value {
			return false
		}
		cfg.TakerCloseMarketMinPrice = value
		return true
	default:
		return false
	}
}

func walletTruthOutcomeKey(outcome string) string {
	return strings.ToLower(strings.TrimSpace(outcome))
}
