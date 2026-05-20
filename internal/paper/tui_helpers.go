package paper

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"Market-bot/internal/core"
)

// ─── Design Tokens ────────────────────────────────────────────────────────────
// True-color palette — falls back gracefully to nearest ANSI in 256-color terms.
var (
	clrBrand   = lipgloss.Color("#7C3AED") // purple  – brand / title
	clrTeal    = lipgloss.Color("#06B6D4") // cyan    – ETH, section labels
	clrEmerald = lipgloss.Color("#10B981") // green   – positive / bid
	clrRose    = lipgloss.Color("#EF4444") // red     – negative / ask / kill
	clrAmber   = lipgloss.Color("#F59E0B") // amber   – BTC / warning
	clrOrange  = lipgloss.Color("#F97316") // orange  – SOL / mid-warning
	clrBlue    = lipgloss.Color("#60A5FA") // blue    – XRP
	clrSlate   = lipgloss.Color("#6B7280") // slate   – muted text
	clrWhite   = lipgloss.Color("#F3F4F6") // near-white – primary text
	clrDim     = lipgloss.Color("#9CA3AF") // dim     – labels / secondary

	// ── Plain text styles (names kept for package-internal compatibility) ──
	styleRed     = lipgloss.NewStyle().Foreground(clrRose)
	styleGreen   = lipgloss.NewStyle().Foreground(clrEmerald)
	styleYellow  = lipgloss.NewStyle().Foreground(clrAmber)
	styleMagenta = lipgloss.NewStyle().Foreground(clrOrange) // SOL
	styleCyan    = lipgloss.NewStyle().Foreground(clrTeal)
	styleWhite   = lipgloss.NewStyle().Foreground(clrWhite)
	styleMuted   = lipgloss.NewStyle().Foreground(clrSlate)
	styleDimmed  = lipgloss.NewStyle().Foreground(clrDim)
	styleBold    = lipgloss.NewStyle().Bold(true).Foreground(clrWhite)

	// Kill banner
	styleBgRedBold = lipgloss.NewStyle().
			Background(clrRose).
			Foreground(lipgloss.Color("#FFFFFF")).
			Bold(true).
			Padding(0, 2)

	styleBoldGreen = lipgloss.NewStyle().Bold(true).Foreground(clrBlue) // XRP

	// Per-asset border colors for market panels
	assetBorderColors = map[string]lipgloss.Color{
		"BTC": clrAmber,
		"ETH": clrTeal,
		"SOL": clrOrange,
		"XRP": clrBlue,
	}
)

const (
	defaultMaxOrderHistory  = 50
	defaultMaxRoundHistory  = 50
	defaultMaxEventHistory  = 250
	defaultTwoColRoundRows  = 4
	defaultOneColRoundRows  = 3
	defaultTwoColOrderRows  = 12
	defaultOneColOrderRows  = 10
	defaultTwoColEventRows  = 4
	defaultOneColEventRows  = 3
	recentQuoteDisplayGrace = 10 * time.Second
	terminalBidFloor        = 0.985
	terminalAskCeil         = 0.015
	inventoryDustCutoff     = 0.01
	showOnChainInventory    = true
	showWalletTruthPanels   = false
)

// ─── Asset style helpers ──────────────────────────────────────────────────────

func getAssetStyle(id string) lipgloss.Style {
	switch marketAssetID(id) {
	case "BTC":
		return styleYellow
	case "ETH":
		return styleCyan
	case "SOL":
		return styleMagenta
	case "XRP":
		return styleBoldGreen.UnsetBold()
	default:
		return styleWhite
	}
}

func marketAssetID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "UNKNOWN"
	}
	for _, sep := range []string{"#", ":", "/"} {
		if idx := strings.Index(id, sep); idx > 0 {
			return strings.ToUpper(strings.TrimSpace(id[:idx]))
		}
	}
	return strings.ToUpper(id)
}

func marketSortRank(id string) int {
	switch marketAssetID(id) {
	case "BTC":
		return 0
	case "ETH":
		return 1
	case "SOL":
		return 2
	case "XRP":
		return 3
	case "UNKNOWN":
		return 5
	default:
		return 4
	}
}

func outcomeSortRank(outcome string) int {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "up", "yes":
		return 0
	case "down", "no":
		return 1
	default:
		return 2
	}
}

func outcomeSortLess(a, b string) bool {
	rankA := outcomeSortRank(a)
	rankB := outcomeSortRank(b)
	if rankA != rankB {
		return rankA < rankB
	}
	return strings.TrimSpace(a) < strings.TrimSpace(b)
}

func orderedMarketIDs(ids []string) []string {
	sorted := append([]string(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool {
		ri := marketSortRank(sorted[i])
		rj := marketSortRank(sorted[j])
		if ri != rj {
			return ri < rj
		}
		return sorted[i] < sorted[j]
	})
	return sorted
}

func marketDisplayLabel(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "UNKNOWN"
	}
	return id
}

func truncateMarketLabel(id string, maxLen int) string {
	label := marketDisplayLabel(id)
	if maxLen > 0 && len(label) > maxLen {
		return label[:maxLen]
	}
	return label
}

func truncateText(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	if maxLen == 1 {
		return s[:1]
	}
	return s[:maxLen-1] + "…"
}

func orderHistoryMarketLabel(marketID, marketSlug string) string {
	if suffix := marketTimeSuffix(marketSlug); suffix != "" {
		return suffix
	}
	if suffix := marketTimeSuffix(marketID); suffix != "" {
		return suffix
	}
	return marketDisplayLabel(marketID)
}

func marketTimeSuffix(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return ""
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '-' || r == '#' || r == ':' || r == '/'
	})
	if len(parts) == 0 {
		return ""
	}
	suffix := strings.TrimSpace(parts[len(parts)-1])
	if suffix == "" {
		return ""
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return suffix
}

func resolveOrderHistoryMarketSlug(entry OrderHistoryEntry, markets map[string]*MarketData) string {
	if slug := strings.TrimSpace(entry.MarketSlug); slug != "" {
		return slug
	}
	marketID := strings.TrimSpace(entry.MarketID)
	if marketID == "" || markets == nil {
		return ""
	}
	if market, ok := markets[marketID]; ok && market != nil {
		return strings.TrimSpace(market.Slug)
	}
	return ""
}

func marginStyle(pct float64) lipgloss.Style {
	switch {
	case pct >= 3:
		return styleGreen
	case pct >= 2:
		return styleYellow
	case pct < 1:
		return styleRed
	default:
		return styleWhite
	}
}

func recentDisplayQuote(current, lastGood float64, age time.Duration, cleared bool) float64 {
	if current > 0 {
		return current
	}
	if cleared {
		return 0
	}
	if age <= recentQuoteDisplayGrace && lastGood > 0 {
		return lastGood
	}
	return 0
}

func signedDollar(amount float64) string {
	if amount < 0 {
		return fmt.Sprintf("-$%.2f", math.Abs(amount))
	}
	return fmt.Sprintf("+$%.2f", amount)
}

func formatDrawdownCash(amount float64) string {
	if math.Abs(amount) < 0.0001 {
		return "$0.00"
	}
	if amount < 0 {
		amount = math.Abs(amount)
	}
	return fmt.Sprintf("-$%.2f", amount)
}

func formatBinanceSignalPrice(symbol string, price float64) string {
	if price <= 0 {
		return "--"
	}

	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	switch {
	case strings.HasPrefix(symbol, "XRP"):
		return fmt.Sprintf("%.4f", price)
	case price >= 10:
		return fmt.Sprintf("%.2f", price)
	case price >= 1:
		return fmt.Sprintf("%.4f", price)
	case price >= 0.1:
		return fmt.Sprintf("%.4f", price)
	default:
		return fmt.Sprintf("%.5f", price)
	}
}

func renderTradingHoursStatus(mode string, now time.Time) string {
	mode, valid := core.NormalizeTradingHoursMode(mode)
	localNow := core.LocalTime(now)
	localClock := localNow.Format("Mon 2006-01-02 15:04:05 MST")
	usNow := core.USTime(now)
	usClock := usNow.Format("Mon 2006-01-02 15:04:05 MST")

	if !valid {
		return styleRed.Render("Jakarta time " + localClock + "  ·  Invalid Jakarta Gate (use HH:MM-HH:MM)")
	}

	if mode == core.TradingHoursModeOff {
		return styleDimmed.Render("Jakarta time " + localClock + "  ·  Trading Gate OFF (24/7)")
	}

	if mode == core.TradingHoursModeWeekdays {
		if core.IsLocalWeekday(now) {
			return styleGreen.Render("Jakarta time " + localClock + "  ·  Weekday Gate OPEN (trading enabled)")
		}
		return styleRed.Render("Jakarta time " + localClock + "  ·  Weekday Gate CLOSED (weekend, trading blocked)")
	}

	if mode == core.TradingHoursModeUSOpen {
		if core.IsUSMarketOpen(now) {
			return styleGreen.Render("US time " + usClock + "  ·  US Market Gate OPEN (trading enabled)")
		}
		return styleRed.Render("US time " + usClock + "  ·  US Market Gate CLOSED (outside hours, trading blocked)")
	}

	if core.IsTradingHourOpen(now, mode) {
		return styleGreen.Render(fmt.Sprintf("Jakarta time %s  ·  Jakarta Gate [%s] OPEN", localClock, mode))
	}
	return styleRed.Render(fmt.Sprintf("Jakarta time %s  ·  Jakarta Gate [%s] CLOSED", localClock, mode))
}

func walletTruthInventoryDisplayShares(wt WalletTruthPosition) (float64, bool) {
	switch {
	case displayableInventoryShares(wt.OnChainShares):
		if wt.ResolutionStatus == "resolved" && !wt.IsWinner && !wt.Redeemable {
			return 0, false
		}
		return wt.OnChainShares, true
	case displayableInventoryShares(wt.LocalShares) && wt.ResolutionStatus != "resolved":
		return wt.LocalShares, true
	default:
		return 0, false
	}
}

func displayableInventoryShares(qty float64) bool {
	return qty >= inventoryDustCutoff-1e-9
}

func walletTruthInventoryStatus(wt WalletTruthPosition) string {
	switch {
	case !displayableInventoryShares(wt.OnChainShares) && displayableInventoryShares(wt.LocalShares) && wt.ResolutionStatus != "resolved":
		return styleYellow.Render("[SYNCING]")
	case wt.Redeemable:
		return styleGreen.Render("[REDEEMABLE]")
	case wt.IsWinner:
		return styleGreen.Render("[WINNER]")
	case wt.ResolutionStatus == "resolved":
		return styleRed.Render("[LOSER]")
	default:
		return styleYellow.Render("[OPEN]")
	}
}

func marketLooksPastEndFromID(marketID string, now time.Time) bool {
	if endTime, err := ParseEndTimeFromSlug(strings.TrimSpace(marketID)); err == nil && !endTime.IsZero() {
		return now.After(endTime)
	}

	parts := strings.Split(marketID, "-")
	if len(parts) <= 1 {
		return false
	}
	lastPart := parts[len(parts)-1]
	ts, err := strconv.ParseInt(lastPart, 10, 64)
	if err != nil || ts <= 1000000000 {
		return false
	}
	return now.After(time.Unix(ts, 0))
}

func looksTerminalBook(outcomes []string, bids, asks map[string]float64) bool {
	if len(outcomes) == 0 {
		return false
	}

	sawExtreme := false
	for _, outcome := range outcomes {
		bid := bids[outcome]
		ask := asks[outcome]

		if bid > 0 && bid < terminalBidFloor {
			return false
		}
		if ask > 0 && ask < terminalBidFloor && ask > terminalAskCeil {
			return false
		}
		if bid >= terminalBidFloor || ask >= terminalBidFloor || (ask > 0 && ask <= terminalAskCeil) {
			sawExtreme = true
		}
	}

	return sawExtreme
}
func makePanel(innerWidth int, borderColor lipgloss.Color, content string) string {
	if innerWidth < 4 {
		innerWidth = 4
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 1).
		Width(innerWidth).
		MaxWidth(innerWidth).
		Render(content)
}

func sectionHeader(icon, label string, color lipgloss.Color) string {
	return lipgloss.NewStyle().Bold(true).Foreground(color).
		Render(icon + " " + label)
}

func renderBar(pct float64, width int) string {
	if width < 1 {
		return ""
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	n := int(pct * float64(width))
	if n > width {
		n = width
	}
	return styleGreen.Render(strings.Repeat("█", n)) +
		styleMuted.Render(strings.Repeat("░", width-n))
}

// latencyDot returns a colored ● for a latency value.
func latencyDot(d time.Duration, warnMs, critMs int64) (string, lipgloss.Style) {
	ms := d.Milliseconds()
	st := styleGreen
	if d == 0 {
		st = styleMuted
	} else if ms >= critMs {
		st = styleRed
	} else if ms >= warnMs {
		st = styleYellow
	}
	return "●", st
}
