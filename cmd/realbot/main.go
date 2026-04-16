package main

import (
	"log"
	"os"
	"os/exec"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/strategy"
)

const (
	UseLiveUI                        = true // Set to false for traditional logging
	paperArbModeTaker                = "taker"
	paperArbModeLaddered             = "laddered-taker"
	paperArbModeBinanceGap           = "binance-gap"
	paperArbModeCopytrade            = "copytrade"
	paperArbModeMaker                = "maker"
	realbotExecutionModeTakerClose   = "taker-close"
	terminalBidFloor                 = 0.985
	terminalAskCeil                  = 0.015
	realbotExecQuoteTimeout          = 1500 * time.Millisecond
	realbotOrderWarmTimeout          = 1500 * time.Millisecond
	realbotTakerCloseRESTTimeout     = 1200 * time.Millisecond
	realbotWSWarnInterval            = 10 * time.Second
	realbotWSForceReconnect          = 10 * time.Second
	realbotMergeTimeout              = 120 * time.Second
	realbotCleanupVerifyTTL          = 20 * time.Second
	realbotFastVerifyTTL             = 6 * time.Second
	minOnChainActionShares           = 0.01
	realbotUIRefreshInterval         = 1000 * time.Millisecond
	realbotMainLoopInterval          = 50 * time.Millisecond
	realbotDecisionLoopInterval      = 100 * time.Millisecond
	realbotCopytradeLoopIntervalMin  = 100 * time.Millisecond
	realbotCopytradeLoopIntervalMax  = 250 * time.Millisecond
	realbotCopytradeUIRefreshMin     = 1000 * time.Millisecond
	realbotCopytradeUIRefreshMax     = 1000 * time.Millisecond
	realbotCopytradeRetryQueueCap    = 256
	realbotCopytradeRetryMaxAge      = 20 * time.Second
	realbotFillPollInterval          = 150 * time.Millisecond
	realbotTakerCloseQuoteRefresh    = 500 * time.Millisecond
	realbotTakerCloseLogInterval     = 5 * time.Second
	realbotTakerCloseLocalMaxAge     = 350 * time.Millisecond
	realbotRedeemConfirmTimeout      = 120 * time.Second
	realbotRedeemSubmitTimeout       = 20 * time.Second
	realbotRedeemProbeTimeout        = 10 * time.Second
	realbotRedeemRetryInterval       = 3 * time.Second
	realbotWalletTruthLogMinDelta    = 0.25
	realbotMaxSaneOutcomeSpread      = 0.10
	realbotMaxSaneAskPairSum         = 1.10
	realbotMinSaneBidPairSum         = 0.90
	realbotExecutionGuardQuoteMaxAge = 1500 * time.Millisecond
	realbotBalanceSyncInterval       = 60 * time.Second
	realbotBalanceSyncTimeout        = 8 * time.Second
	realbotPositionSyncInterval      = 5 * time.Second
	realbotPositionSyncTimeout       = 5 * time.Second
	realbotHealthProbeInterval       = 20 * time.Second
	realbotHealthProbeTimeout        = 3 * time.Second
	realbotMakerQuoteStep            = 0.001
	realbotMakerBaseOffset           = 0.008
	realbotMakerInventorySkewStep    = 0.020
	realbotMakerInventoryTargetMult  = 2.5
	realbotMakerInventoryCapMult     = 5.0
	realbotMakerQuoteSizeSkewFactor  = 0.75
	realbotMakerRequoteInterval      = 500 * time.Millisecond
	realbotMakerMinQuoteValue        = 5.0
	realbotMakerCashUsagePerOutcome  = 0.35
	realbotBatchBuyConfirmTimeout    = 1500 * time.Millisecond
	realbotBuyAttributionTimeout     = 12 * time.Second
	realbotMinDirectOrderValue       = 1.0
	realbotWalletTruthEventMinDelta  = 2.0
	binanceGapMaxSlippageCents       = 1.0
	ladderedTakerMaxPairSum          = 1.25
	ladderedTakerMinSkew             = 0.02
	ladderedTakerHedgeShareRatio     = 0.35
	ladderedTakerMinAsk              = 0.01
	ladderedTakerMaxAsk              = 0.99
)

var globalResWatcher *api.ResolutionWatcher
var runEntrypoint func() error

var realbotMakerStrategyParams = strategy.MakerParams{
	QuoteStep:           realbotMakerQuoteStep,
	DefaultQuoteGap:     realbotMakerBaseOffset,
	InventorySkewStep:   realbotMakerInventorySkewStep,
	QuoteSizeSkewFactor: realbotMakerQuoteSizeSkewFactor,
	CashUsagePerOutcome: realbotMakerCashUsagePerOutcome,
	MinQuoteValue:       realbotMakerMinQuoteValue,
}

func main() {
	if runEntrypoint == nil {
		cmd := exec.Command("go", append([]string{"run", "./cmd/realbot"}, os.Args[1:]...)...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		if err := cmd.Run(); err != nil {
			log.Fatalf("Error: %v", err)
		}
		return
	}

	if err := runEntrypoint(); err != nil {
		log.Fatalf("Error: %v", err)
	}
}
