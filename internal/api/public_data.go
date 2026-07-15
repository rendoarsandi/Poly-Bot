package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var walletAddressPattern = regexp.MustCompile(`^0x[a-fA-F0-9]{40}$`)

type PublicTrade struct {
	ProxyWallet     string  `json:"proxyWallet"`
	Side            string  `json:"side"`
	Asset           string  `json:"asset"`
	ConditionID     string  `json:"conditionId"`
	Size            float64 `json:"size"`
	Price           float64 `json:"price"`
	Timestamp       int64   `json:"timestamp"`
	Title           string  `json:"title"`
	Slug            string  `json:"slug"`
	Icon            string  `json:"icon"`
	EventSlug       string  `json:"eventSlug"`
	Outcome         string  `json:"outcome"`
	OutcomeIndex    int     `json:"outcomeIndex"`
	Name            string  `json:"name"`
	Pseudonym       string  `json:"pseudonym"`
	Bio             string  `json:"bio"`
	ProfileImage    string  `json:"profileImage"`
	ProfileImageOpt string  `json:"profileImageOptimized"`
	TransactionHash string  `json:"transactionHash"`
	ObservedAt      int64   `json:"-"`
	Source          string  `json:"-"`
	SignalID        string  `json:"-"`
}

type PublicProfile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Referral    string `json:"referral"`
	Pseudonym   string `json:"pseudonym"`
	ProxyWallet string `json:"proxyWallet"`
	Bio         string `json:"bio"`
}

type PublicActivitySnapshot struct {
	Trades          []PublicTrade
	Positions       []Position
	TradesErr       error
	PositionsErr    error
	PositionsCached bool
}

func NormalizeWalletAddress(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.HasPrefix(strings.ToLower(raw), "0x") {
		raw = "0x" + raw
	}
	return strings.ToLower(raw)
}

func IsWalletAddress(raw string) bool {
	return walletAddressPattern.MatchString(strings.TrimSpace(raw))
}

func normalizeProfileSearchQuery(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	raw = strings.TrimPrefix(raw, "@")
	if strings.Contains(raw, "://") {
		if parsed, err := url.Parse(raw); err == nil {
			path := strings.Trim(parsed.Path, "/")
			if path != "" {
				parts := strings.Split(path, "/")
				raw = parts[len(parts)-1]
			}
		}
	}
	raw = strings.TrimPrefix(raw, "@")
	return strings.TrimSpace(raw)
}

func (c *RestClient) GetPublicPositions(ctx context.Context, user string, markets []string, sizeThreshold float64, limit int) ([]Position, error) {
	user = NormalizeWalletAddress(user)
	if !IsWalletAddress(user) {
		return nil, fmt.Errorf("invalid wallet address %q", user)
	}
	if sizeThreshold < 0 {
		sizeThreshold = 0
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	u, err := url.Parse("https://data-api.polymarket.com/positions")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("user", user)
	q.Set("sizeThreshold", fmt.Sprintf("%.4f", sizeThreshold))
	q.Set("limit", fmt.Sprintf("%d", limit))
	if len(markets) > 0 {
		q.Set("market", strings.Join(markets, ","))
	}
	q.Set("_nc", fmt.Sprintf("%d", time.Now().UnixNano()))
	u.RawQuery = q.Encode()

	resp, err := doGETWithRetry(ctx, u.String())
	if err != nil {
		return nil, fmt.Errorf("failed to get public positions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return []Position{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
		return nil, fmt.Errorf("get public positions failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var positions []Position
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&positions); err != nil {
		return nil, fmt.Errorf("failed to decode public positions: %w", err)
	}
	return positions, nil
}

func (c *RestClient) GetPublicTrades(ctx context.Context, user string, markets []string, limit int) ([]PublicTrade, error) {
	return c.GetPublicTradesPage(ctx, user, markets, limit, 0)
}

func (c *RestClient) GetPublicTradesPage(ctx context.Context, user string, markets []string, limit int, offset int) ([]PublicTrade, error) {
	user = NormalizeWalletAddress(user)
	if !IsWalletAddress(user) {
		return nil, fmt.Errorf("invalid wallet address %q", user)
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 10000 {
		limit = 10000
	}

	u, err := url.Parse("https://data-api.polymarket.com/trades")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("user", user)
	q.Set("limit", fmt.Sprintf("%d", limit))
	if offset > 0 {
		q.Set("offset", fmt.Sprintf("%d", offset))
	}
	q.Set("takerOnly", "false")
	if len(markets) > 0 {
		q.Set("market", strings.Join(markets, ","))
	}
	q.Set("_nc", fmt.Sprintf("%d", time.Now().UnixNano()))
	u.RawQuery = q.Encode()

	resp, err := doGETWithRetry(ctx, u.String())
	if err != nil {
		return nil, fmt.Errorf("failed to get public trades: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return []PublicTrade{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
		return nil, fmt.Errorf("get public trades failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var trades []PublicTrade
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&trades); err != nil {
		return nil, fmt.Errorf("failed to decode public trades: %w", err)
	}
	return trades, nil
}

func (c *RestClient) GetPublicActivitySnapshot(ctx context.Context, user string, markets []string, tradeLimit int, positionSizeThreshold float64, positionLimit int) PublicActivitySnapshot {
	type tradeResult struct {
		trades []PublicTrade
		err    error
	}
	type positionResult struct {
		positions []Position
		err       error
	}

	tradesCh := make(chan tradeResult, 1)
	positionsCh := make(chan positionResult, 1)

	go func() {
		trades, err := c.GetPublicTrades(ctx, user, markets, tradeLimit)
		tradesCh <- tradeResult{trades: trades, err: err}
	}()
	go func() {
		positions, err := c.GetPublicPositions(ctx, user, markets, positionSizeThreshold, positionLimit)
		positionsCh <- positionResult{positions: positions, err: err}
	}()

	tradesRes := <-tradesCh
	positionsRes := <-positionsCh

	return PublicActivitySnapshot{
		Trades:       tradesRes.trades,
		Positions:    positionsRes.positions,
		TradesErr:    tradesRes.err,
		PositionsErr: positionsRes.err,
	}
}

func (c *RestClient) GetPublicActivitySnapshotWithFallback(ctx context.Context, user string, markets []string, tradeLimit int, positionSizeThreshold float64, positionLimit int, cachedPositions []Position, cachedPositionsValid bool, tradeTimeout, positionTimeout time.Duration) PublicActivitySnapshot {
	type tradeResult struct {
		trades []PublicTrade
		err    error
	}
	type positionResult struct {
		positions []Position
		err       error
	}

	tradesCtx := ctx
	tradesCancel := func() {}
	if tradeTimeout > 0 {
		tradesCtx, tradesCancel = context.WithTimeout(ctx, tradeTimeout)
	}
	defer tradesCancel()

	positionsCtx := ctx
	positionsCancel := func() {}
	if positionTimeout > 0 {
		positionsCtx, positionsCancel = context.WithTimeout(ctx, positionTimeout)
	}
	defer positionsCancel()

	tradesCh := make(chan tradeResult, 1)
	positionsCh := make(chan positionResult, 1)

	go func() {
		trades, err := c.GetPublicTrades(tradesCtx, user, markets, tradeLimit)
		tradesCh <- tradeResult{trades: trades, err: err}
	}()
	go func() {
		positions, err := c.GetPublicPositions(positionsCtx, user, markets, positionSizeThreshold, positionLimit)
		positionsCh <- positionResult{positions: positions, err: err}
	}()

	tradesRes := <-tradesCh
	snapshot := PublicActivitySnapshot{
		Trades:    tradesRes.trades,
		TradesErr: tradesRes.err,
	}

	applyPositions := func(res positionResult) {
		if res.err != nil && cachedPositionsValid {
			snapshot.Positions = append([]Position(nil), cachedPositions...)
			snapshot.PositionsErr = nil
			snapshot.PositionsCached = true
			return
		}
		snapshot.Positions = res.positions
		snapshot.PositionsErr = res.err
	}

	select {
	case positionsRes := <-positionsCh:
		applyPositions(positionsRes)
	default:
		if cachedPositionsValid {
			snapshot.Positions = append([]Position(nil), cachedPositions...)
			snapshot.PositionsErr = nil
			snapshot.PositionsCached = true
			return snapshot
		}
		positionsRes := <-positionsCh
		applyPositions(positionsRes)
	}

	return snapshot
}

func (c *RestClient) SearchPublicProfiles(ctx context.Context, query string, limit int) ([]PublicProfile, error) {
	query = normalizeProfileSearchQuery(query)
	if query == "" {
		return nil, fmt.Errorf("profile query is empty")
	}
	if limit <= 0 {
		limit = 5
	}

	u, err := url.Parse(c.GammaURL + "/public-search")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("search_profiles", "true")
	q.Set("search_tags", "false")
	q.Set("limit_per_type", fmt.Sprintf("%d", limit))
	q.Set("optimized", "true")
	u.RawQuery = q.Encode()

	resp, err := doGETWithRetry(ctx, u.String())
	if err != nil {
		return nil, fmt.Errorf("failed to search public profiles: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
		return nil, fmt.Errorf("public profile search failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Profiles []PublicProfile `json:"profiles"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBodySize)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("failed to decode public profile search: %w", err)
	}
	return payload.Profiles, nil
}

func (c *RestClient) ResolvePublicProfileTarget(ctx context.Context, raw string) (wallet string, profile *PublicProfile, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil, fmt.Errorf("copytrade target is empty")
	}

	normalizedWallet := NormalizeWalletAddress(raw)
	if IsWalletAddress(normalizedWallet) {
		return normalizedWallet, nil, nil
	}

	query := normalizeProfileSearchQuery(raw)
	if query == "" {
		return "", nil, fmt.Errorf("copytrade target is empty")
	}

	profiles, err := c.SearchPublicProfiles(ctx, query, 10)
	if err != nil {
		return "", nil, err
	}
	if len(profiles) == 0 {
		return "", nil, fmt.Errorf("no public profile matched %q", raw)
	}

	var fallback *PublicProfile
	lowerQuery := strings.ToLower(query)
	for i := range profiles {
		candidate := &profiles[i]
		if !IsWalletAddress(candidate.ProxyWallet) {
			continue
		}
		if fallback == nil {
			fallback = candidate
		}
		if strings.EqualFold(candidate.Name, query) || strings.EqualFold(candidate.Pseudonym, query) || strings.EqualFold(candidate.Referral, query) {
			return NormalizeWalletAddress(candidate.ProxyWallet), candidate, nil
		}
		name := strings.ToLower(strings.TrimSpace(candidate.Name))
		pseudonym := strings.ToLower(strings.TrimSpace(candidate.Pseudonym))
		referral := strings.ToLower(strings.TrimSpace(candidate.Referral))
		if name == lowerQuery || pseudonym == lowerQuery || referral == lowerQuery {
			return NormalizeWalletAddress(candidate.ProxyWallet), candidate, nil
		}
	}

	if fallback == nil {
		return "", nil, fmt.Errorf("no searchable profile wallet matched %q", raw)
	}
	return "", nil, fmt.Errorf("copytrade target %q is ambiguous; use the wallet address or exact profile handle", raw)
}
