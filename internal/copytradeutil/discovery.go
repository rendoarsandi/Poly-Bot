package copytradeutil

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"Market-bot/internal/api"
	"Market-bot/internal/core"
)

type Target struct {
	Raw    string
	Wallet string
	Label  string
}

type TargetResolver interface {
	ResolvePublicProfileTarget(ctx context.Context, raw string) (wallet string, profile *api.PublicProfile, err error)
}

type MarketDiscoveryClient interface {
	GetPublicPositions(ctx context.Context, user string, markets []string, sizeThreshold float64, limit int) ([]api.Position, error)
	GetPublicTrades(ctx context.Context, user string, markets []string, limit int) ([]api.PublicTrade, error)
	GetMarket(ctx context.Context, slug string) (*api.Market, error)
}

type FindMarketsOptions struct {
	Wallet     string
	MaxMarkets int
	MarketSlug string
	Timeframe  string
	Now        time.Time
}

type RoundDiscovery struct {
	Target  Target
	Markets map[string]*api.Market
}

type RoundDiscoveryOptions struct {
	ArbMode          string
	CopytradeMode    string
	CopytradeTarget  string
	ResolveTimeout   time.Duration
	Resolver         TargetResolver
	FindMarkets      func() map[string]*api.Market
	OnResolvedTarget func(Target)
}

func ResolveTarget(ctx context.Context, resolver TargetResolver, raw string) (Target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Target{}, fmt.Errorf("copytrade target is empty")
	}
	if resolver == nil {
		return Target{}, fmt.Errorf("copytrade target resolver is nil")
	}

	wallet, profile, err := resolver.ResolvePublicProfileTarget(ctx, raw)
	if err != nil {
		return Target{}, err
	}

	label := wallet
	if profile != nil {
		switch {
		case strings.TrimSpace(profile.Name) != "":
			label = profile.Name
		case strings.TrimSpace(profile.Pseudonym) != "":
			label = profile.Pseudonym
		case strings.TrimSpace(profile.Referral) != "":
			label = "@" + strings.TrimPrefix(profile.Referral, "@")
		}
	}

	return Target{
		Raw:    raw,
		Wallet: wallet,
		Label:  label,
	}, nil
}

func DiscoverRound(ctx context.Context, opts RoundDiscoveryOptions) (RoundDiscovery, error) {
	if opts.FindMarkets == nil {
		return RoundDiscovery{}, fmt.Errorf("market finder is nil")
	}

	discovery := RoundDiscovery{}
	if strings.EqualFold(strings.TrimSpace(opts.ArbMode), strings.TrimSpace(opts.CopytradeMode)) {
		resolveCtx := ctx
		cancel := func() {}
		if opts.ResolveTimeout > 0 {
			resolveCtx, cancel = context.WithTimeout(ctx, opts.ResolveTimeout)
		}
		target, err := ResolveTarget(resolveCtx, opts.Resolver, opts.CopytradeTarget)
		cancel()
		if err != nil {
			return RoundDiscovery{}, err
		}
		discovery.Target = target
		if opts.OnResolvedTarget != nil {
			opts.OnResolvedTarget(target)
		}
	}

	discovery.Markets = opts.FindMarkets()
	return discovery, nil
}

func LabelFromHint(slug, title string) string {
	if slug = core.SanitizeString(slug); slug != "" {
		parts := strings.Split(slug, "-")
		if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
			return strings.ToUpper(strings.TrimSpace(parts[0]))
		}
		return strings.ToUpper(slug)
	}
	title = core.SanitizeString(title)
	if title == "" {
		return "COPY"
	}
	title = strings.ToUpper(title)
	if len(title) > 12 {
		title = title[:12]
	}
	return title
}

func ParseEndTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed
	}
	return time.Time{}
}

func MarketAllowed(slug, marketSlug, timeframe string) bool {
	lSlug := strings.ToLower(slug)
	marketSlug = strings.TrimSpace(marketSlug)
	if marketSlug != "" && !strings.EqualFold(marketSlug, "ALL") {
		allowed := false
		for _, s := range strings.Split(marketSlug, ",") {
			s = strings.TrimSpace(strings.ToLower(s))
			if s != "" && strings.Contains(lSlug, s) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}

	timeframe = strings.TrimSpace(timeframe)
	if timeframe != "" && !strings.EqualFold(timeframe, "ALL") {
		allowed := false
		for _, f := range strings.Split(timeframe, ",") {
			f = strings.TrimSpace(strings.ToLower(f))
			if f != "" && (strings.Contains(lSlug, "-"+f+"-") || strings.HasSuffix(lSlug, "-"+f)) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

func MarketSelectable(now, endTime time.Time) bool {
	if endTime.IsZero() {
		return true
	}
	return !now.After(endTime)
}

func BuildMarketFromPosition(pos api.Position) *api.Market {
	if pos.ConditionID == "" || pos.TokenID == "" || pos.Outcome == "" {
		return nil
	}
	market := &api.Market{
		ConditionID: pos.ConditionID,
		Slug:        core.SanitizeString(pos.Slug),
		EndTime:     ParseEndTime(pos.EndDate),
		Tokens: []api.Token{
			{TokenID: pos.TokenID, Outcome: NormalizeOutcome(pos.Outcome)},
		},
	}
	if pos.OppositeAsset != "" && pos.OppositeOutcome != "" {
		market.Tokens = append(market.Tokens, api.Token{
			TokenID: pos.OppositeAsset,
			Outcome: NormalizeOutcome(pos.OppositeOutcome),
		})
	}
	return market
}

func BuildMarketFromTrade(ctx context.Context, client MarketDiscoveryClient, trade api.PublicTrade) *api.Market {
	if client == nil || trade.ConditionID == "" {
		return nil
	}
	market, err := client.GetMarket(ctx, trade.ConditionID)
	if err == nil && market != nil {
		return market
	}
	return nil
}

func FindMarkets(ctx context.Context, client MarketDiscoveryClient, opts FindMarketsOptions) (map[string]*api.Market, error) {
	if client == nil {
		return nil, fmt.Errorf("rest client is nil")
	}
	if opts.MaxMarkets <= 0 {
		opts.MaxMarkets = 4
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}

	found := make(map[string]*api.Market)
	seen := make(map[string]struct{})

	addMarket := func(label string, market *api.Market) bool {
		if market == nil || market.ConditionID == "" {
			return false
		}
		if _, ok := seen[market.ConditionID]; ok {
			return false
		}
		if !MarketSelectable(opts.Now, market.EndTime) {
			return false
		}
		if !MarketAllowed(market.Slug, opts.MarketSlug, opts.Timeframe) {
			return false
		}
		if label == "" {
			label = LabelFromHint(market.Slug, "")
		}
		if _, exists := found[label]; exists {
			fingerprint := strings.TrimPrefix(strings.TrimPrefix(market.ConditionID, "0x"), "0X")
			if len(fingerprint) > 6 {
				fingerprint = fingerprint[:6]
			}
			if fingerprint == "" {
				fingerprint = "mkt"
			}
			label = label + "-" + strings.ToUpper(fingerprint)
		}
		seen[market.ConditionID] = struct{}{}
		found[label] = market
		return len(found) >= opts.MaxMarkets
	}

	positions, posErr := client.GetPublicPositions(ctx, opts.Wallet, nil, minTrackedShares, opts.MaxMarkets*8)
	if posErr == nil {
		for _, pos := range positions {
			if pos.Size <= minTrackedShares {
				continue
			}
			if addMarket(LabelFromHint(pos.Slug, pos.Title), BuildMarketFromPosition(pos)) {
				return found, nil
			}
		}
	}

	trades, tradeErr := client.GetPublicTrades(ctx, opts.Wallet, nil, opts.MaxMarkets*8)
	if tradeErr == nil {
		sort.Slice(trades, func(i, j int) bool {
			return trades[i].Timestamp > trades[j].Timestamp
		})
		for _, trade := range trades {
			if addMarket(LabelFromHint(trade.Slug, trade.Title), BuildMarketFromTrade(ctx, client, trade)) {
				return found, nil
			}
		}
	}

	if len(found) == 0 {
		switch {
		case posErr != nil && tradeErr != nil:
			return nil, fmt.Errorf("positions: %v | trades: %v", posErr, tradeErr)
		case posErr != nil:
			return nil, posErr
		case tradeErr != nil:
			return nil, tradeErr
		}
	}

	return found, nil
}
