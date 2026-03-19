package paper

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type StartupWizardOptions struct {
	Title          string
	ProfileLabel   string
	Settings       TUISettings
	FirstRun       bool
	RequireConfirm bool
}

type startupWizardField struct {
	id      string
	section string
	label   string
	value   string
	help    string
	action  bool
}

type startupWizardModel struct {
	options   StartupWizardOptions
	settings  TUISettings
	cursor    int
	width     int
	height    int
	confirmed bool
}

func RunStartupWizard(opts StartupWizardOptions) (TUISettings, bool, error) {
	model := startupWizardModel{
		options:  opts,
		settings: normalizeStartupWizardSettings(opts.Settings),
		width:    110,
		height:   30,
	}
	prog := tea.NewProgram(model, tea.WithAltScreen())
	finalModel, err := prog.Run()
	if err != nil {
		return opts.Settings, false, err
	}
	final, ok := finalModel.(startupWizardModel)
	if !ok {
		return opts.Settings, false, fmt.Errorf("unexpected startup wizard model %T", finalModel)
	}
	return normalizeStartupWizardSettings(final.settings), final.confirmed, nil
}

func normalizeStartupWizardSettings(s TUISettings) TUISettings {
	s = normalizeTUISettings(s)
	if strings.TrimSpace(s.Exchange) == "" {
		s.Exchange = "polymarket"
	}
	if strings.TrimSpace(s.Timeframe) == "" {
		s.Timeframe = "15m"
	}
	if strings.TrimSpace(s.PaperArbMode) == "" {
		s.PaperArbMode = "taker"
	}
	s.PaperArbMode = strings.ToLower(strings.TrimSpace(s.PaperArbMode))
	if s.PaperArbMode != "maker" {
		s.PaperArbMode = "taker"
	}
	if s.MaxTradeSize < 0 {
		s.MaxTradeSize = 0
	}
	if s.MaxDailyLoss < 0 {
		s.MaxDailyLoss = 0
	}
	if s.Exchange == "kalshi" {
		s.SplitStrategyEnabled = false
	}
	if startupStrategyProfile(s) == "maker" {
		s.TakerCloseMarket = false
	}
	return s
}

func startupStrategyProfile(s TUISettings) string {
	if s.TakerCloseMarket {
		return "taker-close"
	}
	if isMakerSettingsMode(s) {
		return "maker"
	}
	return "taker"
}

func setStartupStrategyProfile(s *TUISettings, profile string) {
	switch profile {
	case "maker":
		s.PaperArbMode = "maker"
		s.TakerCloseMarket = false
	case "taker-close":
		s.PaperArbMode = "taker"
		s.TakerCloseMarket = true
	default:
		s.PaperArbMode = "taker"
		s.TakerCloseMarket = false
	}
}

func cycleString(values []string, current string, delta int) string {
	if len(values) == 0 {
		return current
	}
	idx := 0
	for i, v := range values {
		if strings.EqualFold(v, current) {
			idx = i
			break
		}
	}
	idx = (idx + delta) % len(values)
	if idx < 0 {
		idx += len(values)
	}
	return values[idx]
}

func (m startupWizardModel) visibleFields() []startupWizardField {
	settings := normalizeStartupWizardSettings(m.settings)
	profile := startupStrategyProfile(settings)
	fields := []startupWizardField{
		{
			id:      "exchange",
			section: "Setup",
			label:   "Exchange",
			value:   startupValuePill(settings.Exchange == "kalshi", "kalshi", "polymarket"),
			help:    "Choose the venue before credential setup runs.",
		},
		{
			id:      "market",
			section: "Setup",
			label:   "Market Scope",
			value:   fmt.Sprintf(" %s ", settings.MarketSlug),
			help:    "Pick all tracked assets or a smaller subset.",
		},
		{
			id:      "max-markets",
			section: "Setup",
			label:   "Max Concurrent",
			value:   fmt.Sprintf(" %d ", settings.MaxMarkets),
			help:    "Caps how many markets realbot trades at the same time.",
		},
	}
	if settings.Exchange != "kalshi" {
		fields = append(fields, startupWizardField{
			id:      "timeframe",
			section: "Setup",
			label:   "Timeframe",
			value:   fmt.Sprintf(" %s ", settings.Timeframe),
			help:    "Used when scanning Polymarket expiry buckets.",
		})
	}

	fields = append(fields,
		startupWizardField{
			id:      "profile",
			section: "Strategy",
			label:   "Strategy Mode",
			value:   startupStrategyValue(profile),
			help:    "Cleaner startup choice than mixing maker, split, and close toggles together.",
		},
	)
	if settings.Exchange != "kalshi" && profile == "taker" {
		fields = append(fields, startupWizardField{
			id:      "split",
			section: "Strategy",
			label:   "Split Strategy",
			value:   startupOnOffValue(settings.SplitStrategyEnabled),
			help:    "Enable split inventory only for polymarket taker mode.",
		})
	}

	fields = append(fields,
		startupWizardField{
			id:      "max-trade",
			section: "Risk",
			label:   "Max Trade Size",
			value:   startupMoneyValue(settings.MaxTradeSize),
			help:    "Set 0 to disable the hard cap.",
		},
		startupWizardField{
			id:      "max-loss",
			section: "Risk",
			label:   "Max Daily Loss",
			value:   startupMoneyValue(settings.MaxDailyLoss),
			help:    "Set 0 to rely on the drawdown kill switch only.",
		},
		startupWizardField{
			id:      "start",
			section: "Start",
			label:   "Continue",
			value:   styleGreen.Render(" save and start "),
			help:    "Save these defaults, then continue into exchange credential setup.",
			action:  true,
		},
	)

	return fields
}

func startupValuePill(active bool, activeLabel, inactiveLabel string) string {
	if active {
		return styleGreen.Render(" "+activeLabel+" ") + " " + styleDimmed.Render(inactiveLabel)
	}
	return styleYellow.Render(" "+inactiveLabel+" ") + " " + styleDimmed.Render(activeLabel)
}

func startupOnOffValue(enabled bool) string {
	if enabled {
		return styleGreen.Render("  ON ")
	}
	return styleMuted.Render(" OFF ")
}

func startupMoneyValue(v float64) string {
	if v <= 0 {
		return styleMuted.Render(" disabled ")
	}
	return fmt.Sprintf(" $%.2f ", v)
}

func startupStrategyValue(profile string) string {
	switch profile {
	case "maker":
		return styleGreen.Render(" maker ")
	case "taker-close":
		return styleRed.Render(" taker-close ")
	default:
		return styleYellow.Render(" taker ")
	}
}

func (m *startupWizardModel) clampCursor() {
	fields := m.visibleFields()
	if len(fields) == 0 {
		m.cursor = 0
		return
	}
	if m.cursor < 0 {
		m.cursor = len(fields) - 1
	}
	if m.cursor >= len(fields) {
		m.cursor = 0
	}
}

func (m *startupWizardModel) adjustCurrent(delta int) {
	fields := m.visibleFields()
	if len(fields) == 0 {
		return
	}
	if m.cursor < 0 || m.cursor >= len(fields) {
		m.cursor = 0
	}
	switch fields[m.cursor].id {
	case "exchange":
		m.settings.Exchange = cycleString([]string{"polymarket", "kalshi"}, m.settings.Exchange, delta)
		if m.settings.Exchange == "kalshi" {
			m.settings.SplitStrategyEnabled = false
		}
	case "market":
		m.settings.MarketSlug = cycleString([]string{"ALL", "BTC", "ETH", "SOL", "XRP", "BTC,ETH", "SOL,XRP", "BTC,ETH,SOL"}, m.settings.MarketSlug, delta)
	case "max-markets":
		m.settings.MaxMarkets += delta
		if m.settings.MaxMarkets < 1 {
			m.settings.MaxMarkets = 1
		}
		if m.settings.MaxMarkets > 20 {
			m.settings.MaxMarkets = 20
		}
	case "timeframe":
		m.settings.Timeframe = cycleString([]string{"15m", "5m"}, m.settings.Timeframe, delta)
	case "profile":
		profile := cycleString([]string{"maker", "taker", "taker-close"}, startupStrategyProfile(m.settings), delta)
		setStartupStrategyProfile(&m.settings, profile)
	case "split":
		m.settings.SplitStrategyEnabled = !m.settings.SplitStrategyEnabled
	case "max-trade":
		m.settings.MaxTradeSize += float64(delta) * 5.0
		if m.settings.MaxTradeSize < 0 {
			m.settings.MaxTradeSize = 0
		}
	case "max-loss":
		m.settings.MaxDailyLoss += float64(delta) * 5.0
		if m.settings.MaxDailyLoss < 0 {
			m.settings.MaxDailyLoss = 0
		}
	}
	m.settings = normalizeStartupWizardSettings(m.settings)
	m.clampCursor()
}

func (m startupWizardModel) Init() tea.Cmd { return nil }

func (m startupWizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "Q", "esc":
			m.confirmed = false
			return m, tea.Quit
		case "up", "k":
			m.cursor--
			m.clampCursor()
			return m, nil
		case "down", "j":
			m.cursor++
			m.clampCursor()
			return m, nil
		case "left", "h", "-":
			m.adjustCurrent(-1)
			return m, nil
		case "right", "l", "+":
			m.adjustCurrent(1)
			return m, nil
		case "enter":
			fields := m.visibleFields()
			if len(fields) == 0 {
				return m, nil
			}
			if m.cursor >= 0 && m.cursor < len(fields) && fields[m.cursor].action {
				m.confirmed = true
				return m, tea.Quit
			}
			m.adjustCurrent(1)
			return m, nil
		case "s", "S":
			m.confirmed = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m startupWizardModel) View() string {
	width := m.width
	if width <= 0 {
		width = 110
	}
	inner := width - 6
	if inner < 68 {
		inner = 68
	}

	fields := m.visibleFields()
	cursorStyle := lipgloss.NewStyle().Bold(true).Foreground(clrBrand)
	labelStyle := lipgloss.NewStyle().Foreground(clrDim)
	valueStyle := lipgloss.NewStyle().Bold(true).Foreground(clrWhite)
	helpStyle := lipgloss.NewStyle().Foreground(clrSlate)
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(clrTeal)

	var lines []string
	lastSection := ""
	for i, field := range fields {
		if field.section != lastSection {
			if lastSection != "" {
				lines = append(lines, "")
			}
			lines = append(lines, sectionStyle.Render(field.section))
			lastSection = field.section
		}

		cursor := "  "
		lSt := labelStyle
		vSt := valueStyle
		if i == m.cursor {
			cursor = cursorStyle.Render("> ")
			lSt = lipgloss.NewStyle().Bold(true).Foreground(clrBrand)
		}

		labelPad := 18 - lipgloss.Width(field.label)
		if labelPad < 0 {
			labelPad = 0
		}
		value := "[" + vSt.Render(field.value) + "]"
		lines = append(lines, fmt.Sprintf("%s%s%s  %s", cursor, lSt.Render(field.label), strings.Repeat(" ", labelPad), value))
		lines = append(lines, helpStyle.Render("   "+field.help))
	}

	title := "⚙  " + strings.TrimSpace(m.options.Title)
	if title == "⚙" {
		title = "⚙  STARTUP"
	}
	titleLine := lipgloss.NewStyle().Bold(true).Foreground(clrBrand).Render(title)
	subtitle := m.options.ProfileLabel
	if subtitle == "" {
		subtitle = "review startup settings before live trading begins"
	}
	flags := []string{}
	if m.options.FirstRun {
		flags = append(flags, styleGreen.Render(" first launch "))
	}
	if m.options.RequireConfirm {
		flags = append(flags, styleYellow.Render(" confirmation required "))
	}
	banner := subtitle
	if len(flags) > 0 {
		banner += "  " + strings.Join(flags, " ")
	}

	notes := []string{
		"Advanced tuning stays available later inside the live TUI with [s].",
	}
	if m.settings.Exchange == "kalshi" {
		notes = append(notes, "Kalshi setup will ask for API key and RSA private key next. Split inventory stays off on Kalshi.")
	} else {
		notes = append(notes, "Polymarket setup will use wallet/CLOB credentials next. Split inventory is available in taker mode.")
	}
	switch startupStrategyProfile(m.settings) {
	case "maker":
		notes = append(notes, "Maker mode keeps quoting logic clean by not mixing in startup taker-close or split toggles.")
	case "taker-close":
		notes = append(notes, "Taker-close starts the dedicated close-market path instead of normal maker quoting.")
	default:
		notes = append(notes, "Taker mode can still enable split inventory without turning startup into a separate text prompt.")
	}

	keys := styleDimmed.Render("  [↑↓/jk] Navigate  [←→/+-] Adjust  [enter] Select / start  [s] Start  [q/Esc] Cancel")
	divider := styleMuted.Render("  " + strings.Repeat("─", min(inner-2, 70)))
	body := titleLine + "\n" +
		styleDimmed.Render("  "+banner) + "\n\n" +
		keys + "\n\n" +
		strings.Join(lines, "\n") + "\n\n" +
		divider + "\n" +
		styleDimmed.Render("  "+notes[0]) + "\n" +
		styleDimmed.Render("  "+notes[1]) + "\n" +
		styleDimmed.Render("  "+notes[2])

	return makePanel(inner, clrBrand, body)
}
