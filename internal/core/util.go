package core

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// RestoreTerminal resets the terminal to a safe, usable state.
// It runs "stty sane" to restore canonical mode and echo, then shows the cursor
// and exits any alternate screen buffer. Safe to call multiple times.
func RestoreTerminal() {
	cmd := exec.Command("stty", "sane")
	cmd.Stdin = os.Stdin
	_ = cmd.Run()
	fmt.Print("\033[?25h")   // Show cursor
	fmt.Print("\033[?1049l") // Exit alternate screen buffer if active
	fmt.Println()
}

// SanitizeString removes ASCII control characters (< 0x20) from s to prevent
// terminal escape injection when displaying untrusted strings from the API.
func SanitizeString(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 {
			return -1
		}
		return r
	}, s)
}
