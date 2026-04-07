package copytradeutil

import (
	"context"
	"errors"
	"testing"
	"time"

	"Market-bot/internal/api"
)

type stubTargetResolver struct {
	wallet  string
	profile *api.PublicProfile
	err     error
	calls   int
}

func (s *stubTargetResolver) ResolvePublicProfileTarget(context.Context, string) (string, *api.PublicProfile, error) {
	s.calls++
	return s.wallet, s.profile, s.err
}

type stubDiscoveryClient struct {
	positions    []api.Position
	positionsErr error
	trades       []api.PublicTrade
	tradesErr    error
	markets      map[string]*api.Market
}

func (s stubDiscoveryClient) GetPublicPositions(context.Context, string, []string, float64, int) ([]api.Position, error) {
	return s.positions, s.positionsErr
}

func (s stubDiscoveryClient) GetPublicTrades(context.Context, string, []string, int) ([]api.PublicTrade, error) {
	return s.trades, s.tradesErr
}

func (s stubDiscoveryClient) GetMarket(context.Context, string) (*api.Market, error) {
	panic("use market id in test helper")
}

type keyedDiscoveryClient struct {
	stubDiscoveryClient
}

func (s keyedDiscoveryClient) GetMarket(_ context.Context, slug string) (*api.Market, error) {
	if market, ok := s.markets[slug]; ok {
		return market, nil
	}
	return nil, errors.New("missing market")
}

func TestResolveTarget(t *testing.T) {
	resolver := &stubTargetResolver{
		wallet: "0xabc",
		profile: &api.PublicProfile{
			Name: "Trader",
		},
	}
	got, err := ResolveTarget(context.Background(), resolver, "  @target ")
	if err != nil {
		t.Fatalf("ResolveTarget returned error: %v", err)
	}
	if got.Raw != "@target" || got.Wallet != "0xabc" || got.Label != "Trader" {
		t.Fatalf("ResolveTarget = %#v", got)
	}

	resolver = &stubTargetResolver{
		wallet:  "0xabc",
		profile: &api.PublicProfile{Referral: "ref"},
	}
	got, err = ResolveTarget(context.Background(), resolver, "target")
	if err != nil {
		t.Fatalf("ResolveTarget referral returned error: %v", err)
	}
	if got.Label != "@ref" {
		t.Fatalf("ResolveTarget referral label = %q, want @ref", got.Label)
	}

	if _, err := ResolveTarget(context.Background(), &stubTargetResolver{}, ""); err == nil {
		t.Fatal("expected empty target to fail")
	}
}

func TestDiscoverRoundNonCopytradeSkipsTargetResolution(t *testing.T) {
	resolver := &stubTargetResolver{wallet: "0xabc"}
	findCalls := 0
	discovery, err := DiscoverRound(context.Background(), RoundDiscoveryOptions{
		ArbMode:       "taker",
		CopytradeMode: "copytrade",
		Resolver:      resolver,
		FindMarkets: func() map[string]*api.Market {
			findCalls++
			return map[string]*api.Market{"BTC": {ConditionID: "cond-1"}}
		},
	})
	if err != nil {
		t.Fatalf("DiscoverRound returned error: %v", err)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d, want 0", resolver.calls)
	}
	if findCalls != 1 {
		t.Fatalf("find calls = %d, want 1", findCalls)
	}
	if discovery.Target.Wallet != "" || len(discovery.Markets) != 1 {
		t.Fatalf("unexpected discovery %#v", discovery)
	}
}

func TestDiscoverRoundCopytradeResolvesTargetBeforeFindingMarkets(t *testing.T) {
	resolver := &stubTargetResolver{wallet: "0xabc"}
	order := make([]string, 0, 2)
	discovery, err := DiscoverRound(context.Background(), RoundDiscoveryOptions{
		ArbMode:         "copytrade",
		CopytradeMode:   "copytrade",
		CopytradeTarget: "@master",
		ResolveTimeout:  time.Second,
		Resolver:        resolver,
		OnResolvedTarget: func(target Target) {
			order = append(order, "resolved:"+target.Wallet)
		},
		FindMarkets: func() map[string]*api.Market {
			order = append(order, "markets")
			return map[string]*api.Market{"BTC": {ConditionID: "cond-1"}}
		},
	})
	if err != nil {
		t.Fatalf("DiscoverRound returned error: %v", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}
	if discovery.Target.Wallet != "0xabc" || len(discovery.Markets) != 1 {
		t.Fatalf("unexpected discovery %#v", discovery)
	}
	if len(order) != 2 || order[0] != "resolved:0xabc" || order[1] != "markets" {
		t.Fatalf("unexpected callback order %#v", order)
	}
}

func TestDiscoverRoundCopytradeReturnsResolveError(t *testing.T) {
	resolver := &stubTargetResolver{err: errors.New("target failed")}
	findCalls := 0
	if _, err := DiscoverRound(context.Background(), RoundDiscoveryOptions{
		ArbMode:         "copytrade",
		CopytradeMode:   "copytrade",
		CopytradeTarget: "@master",
		Resolver:        resolver,
		FindMarkets: func() map[string]*api.Market {
			findCalls++
			return nil
		},
	}); err == nil {
		t.Fatal("expected DiscoverRound to return target resolve error")
	}
	if findCalls != 0 {
		t.Fatalf("find calls = %d, want 0", findCalls)
	}
}

func TestDiscoveryHelpers(t *testing.T) {
	if got := LabelFromHint("btc-15m-range", ""); got != "BTC" {
		t.Fatalf("LabelFromHint(slug) = %q, want BTC", got)
	}
	if got := LabelFromHint("", "Very Long Copytrade Title"); got != "VERY LONG CO" {
		t.Fatalf("LabelFromHint(title) = %q", got)
	}
	if got := LabelFromHint("", ""); got != "COPY" {
		t.Fatalf("LabelFromHint(empty) = %q, want COPY", got)
	}

	if got := ParseEndTime("2026-04-07T12:00:00Z"); got.IsZero() {
		t.Fatal("ParseEndTime should parse RFC3339")
	}
	if got := ParseEndTime("bad"); !got.IsZero() {
		t.Fatalf("ParseEndTime(bad) = %v, want zero", got)
	}

	if !MarketAllowed("btc-15m-range", "BTC,ETH", "15m") {
		t.Fatal("expected btc 15m market to match filters")
	}
	if MarketAllowed("sol-1h-range", "BTC,ETH", "15m") {
		t.Fatal("expected sol 1h market to fail filters")
	}

	now := time.Unix(1000, 0)
	if !MarketSelectable(now, time.Time{}) {
		t.Fatal("expected zero end time to stay selectable")
	}
	if MarketSelectable(now, now.Add(-time.Second)) {
		t.Fatal("expected ended market not to be selectable")
	}
}

func TestBuildMarketFromPosition(t *testing.T) {
	market := BuildMarketFromPosition(api.Position{
		ConditionID:     "cond-1",
		TokenID:         "asset-up",
		Outcome:         "Up",
		Slug:            "btc-15m-range",
		EndDate:         "2026-04-07T12:00:00Z",
		OppositeAsset:   "asset-down",
		OppositeOutcome: "Down",
	})
	if market == nil {
		t.Fatal("expected market from position")
	}
	if len(market.Tokens) != 2 {
		t.Fatalf("expected two tokens, got %d", len(market.Tokens))
	}
	if market.ConditionID != "cond-1" || market.Slug != "btc-15m-range" {
		t.Fatalf("unexpected market %#v", market)
	}
}

func TestFindMarkets(t *testing.T) {
	now := time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC)
	client := keyedDiscoveryClient{
		stubDiscoveryClient: stubDiscoveryClient{
			positions: []api.Position{
				{ConditionID: "cond-1", TokenID: "asset-a", Outcome: "Up", Size: 2, Slug: "btc-15m-range"},
				{ConditionID: "cond-2", TokenID: "asset-b", Outcome: "Up", Size: 0.009, Slug: "eth-15m-range"},
				{ConditionID: "cond-3", TokenID: "asset-c", Outcome: "Up", Size: 3, Slug: "btc-1h-range"},
			},
			trades: []api.PublicTrade{
				{ConditionID: "cond-4", Slug: "btc-15m-breakout", Title: "Breakout", Timestamp: 5},
				{ConditionID: "cond-5", Slug: "btc-15m-breakout", Title: "Breakout", Timestamp: 10},
				{ConditionID: "cond-1", Slug: "btc-15m-range", Title: "Range", Timestamp: 20},
			},
			markets: map[string]*api.Market{
				"cond-4": {ConditionID: "cond-4", Slug: "btc-15m-breakout", EndTime: now.Add(10 * time.Minute)},
				"cond-5": {ConditionID: "cond-5", Slug: "btc-15m-breakout", EndTime: now.Add(20 * time.Minute)},
			},
		},
	}

	found, err := FindMarkets(context.Background(), client, FindMarketsOptions{
		Wallet:     "0xabc",
		MaxMarkets: 3,
		MarketSlug: "BTC",
		Timeframe:  "15m",
		Now:        now,
	})
	if err != nil {
		t.Fatalf("FindMarkets returned error: %v", err)
	}
	if len(found) != 3 {
		t.Fatalf("FindMarkets len = %d, want 3", len(found))
	}
	if _, ok := found["BTC"]; !ok {
		t.Fatalf("expected BTC label from position market, got %#v", found)
	}
	if _, ok := found["BTC-COND-5"]; !ok {
		t.Fatalf("expected duplicate label suffix for cond-5, got %#v", found)
	}
	if _, ok := found["BTC-COND-4"]; !ok {
		t.Fatalf("expected duplicate label suffix for cond-4, got %#v", found)
	}
	if found["BTC-COND-4"].ConditionID != "cond-4" || found["BTC-COND-5"].ConditionID != "cond-5" {
		t.Fatalf("unexpected duplicate label mapping: %#v", found)
	}
}

func TestFindMarketsReturnsErrorsWhenNoMarketsFound(t *testing.T) {
	client := keyedDiscoveryClient{
		stubDiscoveryClient: stubDiscoveryClient{
			positionsErr: errors.New("positions failed"),
			tradesErr:    errors.New("trades failed"),
			markets:      map[string]*api.Market{},
		},
	}
	if _, err := FindMarkets(context.Background(), client, FindMarketsOptions{Wallet: "0xabc"}); err == nil {
		t.Fatal("expected FindMarkets to surface source errors when nothing is found")
	}
}
