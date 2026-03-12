package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/coder/websocket"
	"github.com/joho/godotenv"
)

func main() {
	err := godotenv.Load(".env")
	if err != nil {
		log.Println("No .env file found")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	url := "wss://ws-subscriptions-clob.polymarket.com/ws/user"
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		log.Fatalf("failed to dial: %v", err)
	}
	defer c.Close(websocket.StatusInternalError, "the sky is falling")

	fmt.Printf("Connected!\n")

	apiKey := os.Getenv("POLY_API_KEY")
	secret := os.Getenv("POLY_API_SECRET")
	passphrase := os.Getenv("POLY_PASSPHRASE")

	authMsg := fmt.Sprintf(`{"type":"user","auth":{"key":"%s","secret":"%s","passphrase":"%s"}}`, apiKey, secret, passphrase)

	err = c.Write(ctx, websocket.MessageText, []byte(authMsg))
	if err != nil {
		log.Fatalf("Failed to write: %v", err)
	}

	fmt.Println("Wrote auth message. Reading response...")

	for {
		_, msg, err := c.Read(ctx)
		if err != nil {
			log.Fatalf("Failed to read: %v", err)
		}
		fmt.Printf("Received: %s\n", string(msg))
	}
}
