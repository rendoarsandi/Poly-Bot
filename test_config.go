package main

import (
	"fmt"
	"Market-bot/internal/core"
)

func main() {
	_, err := core.LoadBotConfig("paperbot")
	if err != nil {
		fmt.Printf("paperbot error: %v\n", err)
	} else {
		fmt.Println("paperbot OK")
	}

	_, err2 := core.LoadBotConfig("realbot")
	if err2 != nil {
		fmt.Printf("realbot error: %v\n", err2)
	} else {
		fmt.Println("realbot OK")
	}
}
