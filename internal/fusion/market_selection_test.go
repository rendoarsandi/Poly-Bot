package fusion

import (
	"testing"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
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
			"cond-thin": {Available: true, Complete: true, UpBid: 0.49, UpAsk: 0.56, DownBid: 0.44, DownAsk: 0.51, UpBidDepth: 20, UpAskDepth: 25, DownBidDepth: 20, DownAskDepth: 25, PairBidSum: 0.93, PairAskSum: 1.07, MaxSpread: 0.07},
			"cond-deep": {Available: true, Complete: true, UpBid: 0.50, UpAsk: 0.52, DownBid: 0.48, DownAsk: 0.50, UpBidDepth: 220, UpAskDepth: 240, DownBidDepth: 220, DownAskDepth: 240, PairBidSum: 0.98, PairAskSum: 1.02, MaxSpread: 0.02},
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

func TestChooseLatestMarketsPenalizesIncompleteCandidate(t *testing.T) {
	now := time.Date(2026, 3, 9, 6, 13, 45, 0, time.UTC)
	current := marketForTest("eth-updown-15m-1", "cond-current", now.Add(70*time.Second), true, false, "up", "down")
	incomplete := marketForTest("eth-updown-15m-2", "cond-incomplete", now.Add(16*time.Minute), true, false, "up", "down")
	complete := marketForTest("eth-updown-15m-3", "cond-complete", now.Add(15*time.Minute), true, false, "up", "down")

	selected := chooseLatestMarkets(
		[]api.Market{current, incomplete, complete},
		map[string]*trackedMarket{"ETH": {Market: &current}},
		map[string]marketQuality{
			"cond-incomplete": {Available: false, Complete: false, UpBid: 0.52, UpAsk: 0.0, DownBid: 0.46, DownAsk: 0.47, UpBidDepth: 120, DownAskDepth: 120},
			"cond-complete":   {Available: true, Complete: true, UpBid: 0.51, UpAsk: 0.52, DownBid: 0.48, DownAsk: 0.49, UpBidDepth: 200, UpAskDepth: 200, DownBidDepth: 200, DownAskDepth: 200, PairBidSum: 0.99, PairAskSum: 1.01, MaxSpread: 0.01},
		},
		[]string{"eth"},
		"15m",
		4,
		now,
	)

	if got := selected["ETH"]; got == nil || got.ConditionID != complete.ConditionID {
		t.Fatalf("expected complete market to be selected over incomplete one, got %+v", got)
	}
}

func TestChooseLatestMarketsWithoutCurrentPrefersNearestCycle(t *testing.T) {
	now := time.Date(2026, 3, 9, 6, 14, 0, 0, time.UTC)
	near := marketForTest("sol-updown-15m-near", "cond-near", now.Add(16*time.Minute), true, false, "up", "down")
	far := marketForTest("sol-updown-15m-far", "cond-far", now.Add(31*time.Minute), true, false, "up", "down")

	selected := chooseLatestMarkets(
		[]api.Market{far, near},
		nil,
		map[string]marketQuality{
			"cond-near": {Available: true, Complete: true, UpBid: 0.49, UpAsk: 0.52, DownBid: 0.47, DownAsk: 0.50, UpBidDepth: 80, UpAskDepth: 80, DownBidDepth: 80, DownAskDepth: 80, PairBidSum: 0.96, PairAskSum: 1.02, MaxSpread: 0.03},
			"cond-far":  {Available: true, Complete: true, UpBid: 0.50, UpAsk: 0.51, DownBid: 0.49, DownAsk: 0.50, UpBidDepth: 400, UpAskDepth: 400, DownBidDepth: 400, DownAskDepth: 400, PairBidSum: 0.99, PairAskSum: 1.01, MaxSpread: 0.01},
		},
		[]string{"sol"},
		"15m",
		4,
		now,
	)

	if got := selected["SOL"]; got == nil || got.ConditionID != near.ConditionID {
		t.Fatalf("expected nearest cycle to be selected, got %+v", got)
	}
}

func TestChooseLatestMarketsNearExpiryRotatesToNextCycle(t *testing.T) {
	now := time.Date(2026, 3, 9, 6, 14, 0, 0, time.UTC)
	current := marketForTest("sol-updown-15m-current", "cond-current", now.Add(70*time.Second), true, false, "up", "down")
	next := marketForTest("sol-updown-15m-next", "cond-next", now.Add(16*time.Minute), true, false, "up", "down")
	later := marketForTest("sol-updown-15m-later", "cond-later", now.Add(31*time.Minute), true, false, "up", "down")

	selected := chooseLatestMarkets(
		[]api.Market{later, current, next},
		map[string]*trackedMarket{"SOL": {Market: &current}},
		map[string]marketQuality{
			"cond-next":  {Available: true, Complete: true, UpBid: 0.49, UpAsk: 0.51, DownBid: 0.48, DownAsk: 0.50, UpBidDepth: 120, UpAskDepth: 120, DownBidDepth: 120, DownAskDepth: 120, PairBidSum: 0.97, PairAskSum: 1.01, MaxSpread: 0.02},
			"cond-later": {Available: true, Complete: true, UpBid: 0.50, UpAsk: 0.51, DownBid: 0.49, DownAsk: 0.50, UpBidDepth: 350, UpAskDepth: 350, DownBidDepth: 350, DownAskDepth: 350, PairBidSum: 0.99, PairAskSum: 1.01, MaxSpread: 0.01},
		},
		[]string{"sol"},
		"15m",
		4,
		now,
	)

	if got := selected["SOL"]; got == nil || got.ConditionID != next.ConditionID {
		t.Fatalf("expected next cycle instead of later cycle, got %+v", got)
	}
}

func TestMarketQuoteHealthDetectsMissingAsk(t *testing.T) {
	health := marketQuoteHealth(
		map[string]float64{"Up": 0.52, "Down": 0.46},
		map[string]float64{"Up": 0.0, "Down": 0.47},
	)
	if health.Complete {
		t.Fatalf("expected incomplete health, got %+v", health)
	}
	if len(health.Missing) != 1 || health.Missing[0] != "Up ask" {
		t.Fatalf("expected missing Up ask, got %+v", health)
	}
}

func TestShouldProbeMarketQualities(t *testing.T) {
	if shouldProbeMarketQualities(nil) {
		t.Fatal("expected nil current markets to skip quality probing")
	}
	if shouldProbeMarketQualities(map[string]*trackedMarket{"BTC": nil}) {
		t.Fatal("expected empty tracked entries to skip quality probing")
	}
	if !shouldProbeMarketQualities(map[string]*trackedMarket{"BTC": {Market: &api.Market{ConditionID: "cond"}}}) {
		t.Fatal("expected existing tracked market to enable quality probing")
	}
}

func TestCopyTrackedMarketDeepCopiesMutableState(t *testing.T) {
	now := time.Date(2026, 3, 9, 7, 30, 0, 0, time.UTC)
	original := &trackedMarket{
		Asset:        "SOL",
		Market:       &api.Market{ConditionID: "cond-sol"},
		Bids:         map[string]float64{"Up": 0.49},
		Asks:         map[string]float64{"Up": 0.51},
		DepthBids:    map[string][]paper.MarketLevel{"Up": {{Price: 0.49, Size: 100}}},
		DepthAsks:    map[string][]paper.MarketLevel{"Up": {{Price: 0.51, Size: 120}}},
		UpMidHistory: []timedValue{{At: now, Value: 0.50}},
		EventTimes:   []time.Time{now},
		ScoreHistory: []timedValue{{At: now, Value: 0.02}},
	}

	copy := copyTrackedMarket(original)
	if copy == nil {
		t.Fatal("expected copied market")
	}
	original.Bids["Up"] = 0.10
	original.Asks["Up"] = 0.90
	original.DepthBids["Up"][0].Price = 0.10
	original.UpMidHistory[0].Value = 0.10
	original.EventTimes[0] = now.Add(time.Minute)
	original.ScoreHistory[0].Value = 0.10

	if copy.Bids["Up"] != 0.49 || copy.Asks["Up"] != 0.51 {
		t.Fatalf("expected quote maps to be copied, got bids=%v asks=%v", copy.Bids, copy.Asks)
	}
	if copy.DepthBids["Up"][0].Price != 0.49 {
		t.Fatalf("expected depth levels to be copied, got %+v", copy.DepthBids)
	}
	if copy.UpMidHistory[0].Value != 0.50 || !copy.EventTimes[0].Equal(now) || copy.ScoreHistory[0].Value != 0.02 {
		t.Fatalf("expected histories to be copied, got mids=%+v events=%+v scores=%+v", copy.UpMidHistory, copy.EventTimes, copy.ScoreHistory)
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
