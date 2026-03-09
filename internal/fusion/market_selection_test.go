package fusion

import (
	"testing"
	"time"

	"Market-bot/internal/api"
)

func TestChooseLatestMarketsKeepsCurrentHealthyMarket(t *testing.T) {
	now := time.Date(2026, 3, 9, 6, 10, 0, 0, time.UTC)
	current := marketForTest("btc-updown-15m-1", "cond-current", now.Add(5*time.Minute), true, false, "up", "down")
	next := marketForTest("btc-updown-15m-2", "cond-next", now.Add(20*time.Minute), true, false, "up", "down")

	selected := chooseLatestMarkets(
		[]api.Market{next, current},
		map[string]*trackedMarket{"BTC": {Market: &current}},
		map[string]marketQuality{},
		[]string{"btc"},
		"15m",
		4,
		now,
	)

	if got := selected["BTC"]; got == nil || got.ConditionID != current.ConditionID {
		t.Fatalf("expected current market to be kept, got %+v", got)
	}
}

func TestChooseLatestMarketsRotatesBeforeExpiry(t *testing.T) {
	now := time.Date(2026, 3, 9, 6, 13, 45, 0, time.UTC)
	current := marketForTest("btc-updown-15m-1", "cond-current", now.Add(75*time.Second), true, false, "up", "down")
	next := marketForTest("btc-updown-15m-2", "cond-next", now.Add(16*time.Minute), true, false, "up", "down")

	selected := chooseLatestMarkets(
		[]api.Market{current, next},
		map[string]*trackedMarket{"BTC": {Market: &current}},
		map[string]marketQuality{},
		[]string{"btc"},
		"15m",
		4,
		now,
	)

	if got := selected["BTC"]; got == nil || got.ConditionID != next.ConditionID {
		t.Fatalf("expected next market to be selected, got %+v", got)
	}
}

func TestChooseLatestMarketsRejectsInvalidCandidates(t *testing.T) {
	now := time.Date(2026, 3, 9, 6, 10, 0, 0, time.UTC)
	invalidClosed := marketForTest("eth-updown-15m-bad", "cond-closed", now.Add(5*time.Minute), true, true, "up", "down")
	invalidOutcome := marketForTest("eth-updown-15m-bad2", "cond-bad", now.Add(10*time.Minute), true, false, "yes", "no")
	valid := marketForTest("eth-updown-15m-good", "cond-good", now.Add(15*time.Minute), true, false, "up", "down")

	selected := chooseLatestMarkets(
		[]api.Market{invalidClosed, invalidOutcome, valid},
		nil,
		map[string]marketQuality{},
		[]string{"eth"},
		"15m",
		4,
		now,
	)

	if got := selected["ETH"]; got == nil || got.ConditionID != valid.ConditionID {
		t.Fatalf("expected valid market to survive filtering, got %+v", got)
	}
}

func TestChooseLatestMarketsPrefersMoreLiquidCandidateWhenRotating(t *testing.T) {
	now := time.Date(2026, 3, 9, 6, 13, 45, 0, time.UTC)
	current := marketForTest("btc-updown-15m-1", "cond-current", now.Add(75*time.Second), true, false, "up", "down")
	thin := marketForTest("btc-updown-15m-2", "cond-thin", now.Add(14*time.Minute), true, false, "up", "down")
	deep := marketForTest("btc-updown-15m-3", "cond-deep", now.Add(16*time.Minute), true, false, "up", "down")

	selected := chooseLatestMarkets(
		[]api.Market{current, thin, deep},
		map[string]*trackedMarket{"BTC": {Market: &current}},
		map[string]marketQuality{
			"cond-thin": {Available: true, UpBid: 0.49, UpAsk: 0.56, DownBid: 0.44, DownAsk: 0.51, UpBidDepth: 20, UpAskDepth: 25, DownBidDepth: 20, DownAskDepth: 25},
			"cond-deep": {Available: true, UpBid: 0.50, UpAsk: 0.52, DownBid: 0.48, DownAsk: 0.50, UpBidDepth: 220, UpAskDepth: 240, DownBidDepth: 220, DownAskDepth: 240},
		},
		[]string{"btc"},
		"15m",
		4,
		now,
	)

	if got := selected["BTC"]; got == nil || got.ConditionID != deep.ConditionID {
		t.Fatalf("expected deeper/tighter market to be selected, got %+v", got)
	}
}

func marketForTest(slug, condition string, end time.Time, active, closed bool, outcomes ...string) api.Market {
	tokens := make([]api.Token, 0, len(outcomes))
	for i, outcome := range outcomes {
		tokens = append(tokens, api.Token{TokenID: condition + "-token-" + string(rune('a'+i)), Outcome: outcome})
	}
	return api.Market{
		Active:      active,
		Closed:      closed,
		ConditionID: condition,
		Slug:        slug,
		EndTime:     end,
		Tokens:      tokens,
	}
}
