package botmode

import "strings"

const (
	ArbModeTaker         = "taker"
	ArbModeLadderedTaker = "laddered-taker"
	ArbModeBinanceGap    = "binance-gap"
	ArbModeCopytrade     = "copytrade"
	ArbModeMaker         = "maker"
)

func NormalizeArbMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ArbModeMaker:
		return ArbModeMaker
	case ArbModeCopytrade:
		return ArbModeCopytrade
	case ArbModeLadderedTaker:
		return ArbModeLadderedTaker
	case ArbModeBinanceGap:
		return ArbModeBinanceGap
	default:
		return ArbModeTaker
	}
}

func IsMaker(mode string) bool {
	return NormalizeArbMode(mode) == ArbModeMaker
}

func IsCopytrade(mode string) bool {
	return NormalizeArbMode(mode) == ArbModeCopytrade
}

func IsLadderedTaker(mode string) bool {
	return NormalizeArbMode(mode) == ArbModeLadderedTaker
}

func IsBinanceGap(mode string) bool {
	return NormalizeArbMode(mode) == ArbModeBinanceGap
}

func TakerCloseModeActive(arbMode string, enabled bool) bool {
	return enabled && !IsMaker(arbMode) && !IsCopytrade(arbMode) && !IsBinanceGap(arbMode) && !IsLadderedTaker(arbMode)
}

func SplitStrategyAllowed(arbMode string, takerClose bool, exchange string) bool {
	return NormalizeArbMode(arbMode) == ArbModeTaker &&
		!takerClose &&
		!strings.EqualFold(strings.TrimSpace(exchange), "kalshi")
}

func SplitStrategyActive(arbMode string, takerClose bool, exchange string, enabled bool) bool {
	return enabled && SplitStrategyAllowed(arbMode, takerClose, exchange)
}

func Modes() []string {
	return []string{
		ArbModeTaker,
		ArbModeLadderedTaker,
		ArbModeBinanceGap,
		ArbModeCopytrade,
		ArbModeMaker,
	}
}
