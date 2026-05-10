package setup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"Market-bot/internal/core"
	"Market-bot/internal/trading"
	"github.com/joho/godotenv"
)

var ErrSkipToPolymarket = errors.New("user requested to skip kalshi and use polymarket")

// EnsureRealTradingSetup checks if the environment is ready for real trading.
// It will prompt for missing private keys, auto-derive API keys, and auto-approve allowances.
func EnsureRealTradingSetup(ctx context.Context, cfg *core.Config) (*trading.RealTrader, error) {
	cfg.UseRealTrading()

	for {
		err := cfg.ValidateForRealTrading()
		if err == nil {
			break
		}

		if cfg.Exchange == "kalshi" {
			if err := EnsureKalshiCredentials(cfg); err != nil {
				if errors.Is(err, ErrSkipToPolymarket) {
					cfg.Exchange = "polymarket"
					_ = cfg.SaveSettings()
					continue
				}
				return nil, err
			}
		} else {
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

			rpcURL := cfg.PolygonRPCURL
			if rpcURL == "" {
				fmt.Print("Please enter your Polygon RPC URL (or press Enter for default): ")
				reader := bufio.NewReader(os.Stdin)
				input, _ := reader.ReadString('\n')
				rpcURL = strings.TrimSpace(input)
				if rpcURL == "" {
					rpcURL = "https://polygon-rpc.com/"
				}
			}

			fmt.Println("🔄 Deriving API credentials from your private key and saving to .env...")
			err = UpdatePolymarketCredentials(rpcURL, pk)
			if err != nil {
				return nil, fmt.Errorf("failed to save .env file: %w", err)
			}
			fmt.Println("✅ Credentials derived successfully.")

			// Reload env-backed credentials while preserving any bot/profile-specific
			// runtime settings already loaded from JSON.
			_ = godotenv.Overload()
			cfg.ReloadSecretsFromEnv()
			cfg.PolygonRPCURL = rpcURL // Guarantee it's in memory for the current run
			break
		}
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
		fmt.Printf("⚠️  Warning: failed to auto-approve trading allowances: %v\n", err)
		fmt.Println("⚠️  Live trading may fail. You can switch to paper mode in the UI or fund your wallet with MATIC for gas.")
	} else {
		fmt.Println("✅ On-chain allowances are approved.")
	}

	return trader, nil
}

// EnsureKalshiCredentials prompts the user for Kalshi API and Private Keys if missing and saves them to .env
func EnsureKalshiCredentials(cfg *core.Config) error {
	fmt.Println("\n⚠️ Kalshi credentials missing or incomplete.")

	kalshiKey := cfg.KalshiAPIKey
	if kalshiKey == "" {
		fmt.Print("Please enter your Kalshi API Key (or type 'skip' to use polymarket): ")
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if strings.ToLower(input) == "skip" {
			return ErrSkipToPolymarket
		}
		kalshiKey = input
		if kalshiKey == "" {
			return fmt.Errorf("kalshi api key is required")
		}
	}

	kalshiPK := cfg.KalshiPK
	if kalshiPK == "" {
		fmt.Println("Please enter your Kalshi Private Key (Press Enter on an empty line to finish, or type 'skip' to use polymarket):")
		scanner := bufio.NewScanner(os.Stdin)
		var lines []string
		for scanner.Scan() {
			line := scanner.Text()
			if len(lines) == 0 && strings.ToLower(strings.TrimSpace(line)) == "skip" {
				return ErrSkipToPolymarket
			}
			if strings.TrimSpace(line) == "" {
				break
			}
			lines = append(lines, line)
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("failed to read kalshi private key: %w", err)
		}
		kalshiPK = strings.Join(lines, "\n")
		if kalshiPK == "" {
			return fmt.Errorf("kalshi private key is required")
		}
	}

	fmt.Println("✅ Credentials collected. Saving to .env...")
	err := UpdateKalshiEnvFile(kalshiKey, kalshiPK)
	if err != nil {
		return fmt.Errorf("failed to save .env file: %w", err)
	}

	_ = godotenv.Load()
	cfg.ReloadSecretsFromEnv()
	return nil
}

func UpdateKalshiEnvFile(kalshiKey, kalshiPK string) error {
	envFile := ".env"
	envMap, err := godotenv.Read(envFile)
	if err != nil {
		// If file doesn't exist, create an empty map
		if os.IsNotExist(err) {
			envMap = make(map[string]string)
		} else {
			return err
		}
	}

	if kalshiKey != "" {
		envMap["KALSHI_API_KEY"] = kalshiKey
	}
	if kalshiPK != "" {
		envMap["KALSHI_PK"] = kalshiPK
	}

	return godotenv.Write(envMap, envFile)
}

func UpdatePolymarketCredentials(rpc, pk string) error {
	var creds *APICredentials
	var err error

	if pk != "" {
		creds, err = deriveOrBuildAPIKey(pk)
		if err != nil {
			return fmt.Errorf("failed to derive keys: %w", err)
		}
	}

	return updatePolymarketEnvFile(".env", rpc, pk, creds)
}

func updateEnvFile(pk string, creds *APICredentials) error {
	return updatePolymarketEnvFile(".env", "", pk, creds)
}

func updatePolymarketEnvFile(envFile, rpc, pk string, creds *APICredentials) error {
	lines := []string{}
	if _, err := os.Stat(envFile); err == nil {
		content, err := os.ReadFile(envFile)
		if err == nil {
			lines = strings.Split(string(content), "\n")
		}
	}

	updated := map[string]bool{
		"POLYGON_RPC_URL": false,
		"POLY_PK":         false,
		"POLY_API_KEY":    false,
		"POLY_API_SECRET": false,
		"POLY_PASSPHRASE": false,
	}

	for i, line := range lines {
		for key := range updated {
			if strings.HasPrefix(strings.TrimSpace(line), key+"=") {
				switch key {
				case "POLYGON_RPC_URL":
					if rpc != "" {
						lines[i] = key + "=" + rpc
					}
				case "POLY_PK":
					if pk != "" {
						lines[i] = key + "=" + pk
					}
				case "POLY_API_KEY":
					if pk != "" && creds != nil {
						lines[i] = key + "=" + creds.APIKey
					}
				case "POLY_API_SECRET":
					if pk != "" && creds != nil {
						lines[i] = key + "=" + creds.Secret
					}
				case "POLY_PASSPHRASE":
					if pk != "" && creds != nil {
						lines[i] = key + "=" + creds.Passphrase
					}
				}
				updated[key] = true
			}
		}
	}

	for key, isUpdated := range updated {
		if !isUpdated {
			switch key {
			case "POLYGON_RPC_URL":
				if rpc != "" {
					lines = append(lines, key+"="+rpc)
				}
			case "POLY_PK":
				if pk != "" {
					lines = append(lines, key+"="+pk)
				}
			case "POLY_API_KEY":
				if pk != "" && creds != nil {
					lines = append(lines, key+"="+creds.APIKey)
				}
			case "POLY_API_SECRET":
				if pk != "" && creds != nil {
					lines = append(lines, key+"="+creds.Secret)
				}
			case "POLY_PASSPHRASE":
				if pk != "" && creds != nil {
					lines = append(lines, key+"="+creds.Passphrase)
				}
			}
		}
	}

	return os.WriteFile(envFile, []byte(strings.Join(lines, "\n")), 0600)
}
