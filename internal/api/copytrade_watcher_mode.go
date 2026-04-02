package api

import "strings"

const (
	CopytradeMinedWatcherModeFallback = "fallback"
	CopytradeMinedWatcherModeAlways   = "always"
	CopytradeMinedWatcherModeOff      = "off"
)

// NormalizeCopytradeMinedWatcherMode keeps env/config parsing tolerant while
// defaulting to the low-request fallback mode.
func NormalizeCopytradeMinedWatcherMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", CopytradeMinedWatcherModeFallback:
		return CopytradeMinedWatcherModeFallback
	case CopytradeMinedWatcherModeAlways, "on", "true", "1", "enabled":
		return CopytradeMinedWatcherModeAlways
	case CopytradeMinedWatcherModeOff, "false", "0", "disabled", "none":
		return CopytradeMinedWatcherModeOff
	default:
		return CopytradeMinedWatcherModeFallback
	}
}

// ShouldEnableCopytradeMinedWatcher decides whether the on-chain mined watcher
// should run for copytrade. The mined watcher is the reliable path because the
// low-CU pending watcher only sees a subset of target activity when orders are
// relayed through exchange/router wallets.
func ShouldEnableCopytradeMinedWatcher(mode, pendingWSURL string) bool {
	switch NormalizeCopytradeMinedWatcherMode(mode) {
	case CopytradeMinedWatcherModeAlways:
		return true
	case CopytradeMinedWatcherModeOff:
		return false
	default:
		return true
	}
}
