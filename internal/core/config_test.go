package core

import (
	"os"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	// Set dummy env vars
	os.Setenv("MARKET_SLUG", "test-market")
	defer os.Unsetenv("MARKET_SLUG")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.MarketSlug != "test-market" {
		t.Errorf("Expected MarketSlug 'test-market', got '%s'", cfg.MarketSlug)
	}
}
