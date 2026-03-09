package main

import (
	"log"

	"Market-bot/internal/fusion"
)

func main() {
	if err := fusion.Run(); err != nil {
		log.Fatalf("fusionbot failed: %v", err)
	}
}
