package core

import (
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	PK             string
	APIKey         string
	APISecret      string
	APIPassphrase  string
	PolygonRPCURL  string
	MarketSlug     string
}

func LoadConfig() (*Config, error) {
	// Load .env file if it exists, but don't fail if it doesn't (env vars might be set already)
	_ = godotenv.Load()

	return &Config{
		PK:             os.Getenv("PK"),
		APIKey:         os.Getenv("API_KEY"),
		APISecret:      os.Getenv("API_SECRET"),
		APIPassphrase:  os.Getenv("API_PASSPHRASE"),
		PolygonRPCURL:  os.Getenv("POLYGON_RPC_URL"),
		MarketSlug:     os.Getenv("MARKET_SLUG"),
	}, nil
}
