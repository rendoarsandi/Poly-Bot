package setup

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"Market-bot/internal/core"
	"Market-bot/internal/trading"
	"github.com/joho/godotenv"
)

// EnsureRealTradingSetup checks if the environment is ready for real trading.
// It will prompt for missing private keys, auto-derive API keys, and auto-approve allowances.
func EnsureRealTradingSetup(ctx context.Context, cfg *core.Config) (*trading.RealTrader, error) {
	cfg.UseRealTrading()

	err := cfg.ValidateForRealTrading()
	if err != nil {
		fmt.Println("\n⚠️ Polymarket credentials missing or incomplete.")

		pk := cfg.PK
		if pk == "" {
			fmt.Print("Please enter your Polygon Private Key (starts with 0x): ")
			reader := bufio.NewReader(os.Stdin)
			input, _ := reader.ReadString('\n')
			pk = strings.TrimSpace(input)
			if pk == "" {
				return nil, fmt.Errorf("private key is required for real trading")
			}
		}

		fmt.Println("🔄 Deriving API credentials from your private key...")
		creds, deriveErr := deriveOrBuildAPIKey(pk)
		if deriveErr != nil {
			return nil, fmt.Errorf("failed to derive API credentials: %w", deriveErr)
		}

		fmt.Println("✅ Credentials derived successfully. Saving to .env...")
		err = updateEnvFile(pk, creds)
		if err != nil {
			return nil, fmt.Errorf("failed to save .env file: %w", err)
		}

		// Reload env-backed credentials while preserving any bot/profile-specific
		// runtime settings already loaded from JSON.
		_ = godotenv.Load()
		cfg.ReloadSecretsFromEnv()
	}

	trader, err := trading.NewRealTrader(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create real trader: %w", err)
	}

	// Now check allowances silently, or prompt like diagnose does
	fmt.Println("🔄 Checking on-chain allowances for trading...")
	// We run ApproveTrading to ensure all 4 permissions are granted (USDC & CTF for both exchanges)
	// It only executes transactions if permissions are missing
	_, err = trader.ApproveTrading(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to auto-approve trading allowances: %w", err)
	}

	return trader, nil
}

func updateEnvFile(pk string, creds *APICredentials) error {
	envFile := ".env"
	lines := []string{}

	if _, err := os.Stat(envFile); err == nil {
		content, err := os.ReadFile(envFile)
		if err == nil {
			lines = strings.Split(string(content), "\n")
		}
	}

	// Update or add lines
	updated := map[string]bool{
		"POLY_PK":         false,
		"POLY_API_KEY":    false,
		"POLY_API_SECRET": false,
		"POLY_PASSPHRASE": false,
	}

	for i, line := range lines {
		for key := range updated {
			if strings.HasPrefix(strings.TrimSpace(line), key+"=") {
				switch key {
				case "POLY_PK":
					lines[i] = key + "=" + pk
				case "POLY_API_KEY":
					lines[i] = key + "=" + creds.APIKey
				case "POLY_API_SECRET":
					lines[i] = key + "=" + creds.Secret
				case "POLY_PASSPHRASE":
					lines[i] = key + "=" + creds.Passphrase
				}
				updated[key] = true
			}
		}
	}

	// Add missing keys
	for key, isUpdated := range updated {
		if !isUpdated {
			switch key {
			case "POLY_PK":
				lines = append(lines, key+"="+pk)
			case "POLY_API_KEY":
				lines = append(lines, key+"="+creds.APIKey)
			case "POLY_API_SECRET":
				lines = append(lines, key+"="+creds.Secret)
			case "POLY_PASSPHRASE":
				lines = append(lines, key+"="+creds.Passphrase)
			}
		}
	}

	return os.WriteFile(envFile, []byte(strings.Join(lines, "\n")), 0600)
}
