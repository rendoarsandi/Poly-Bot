package main

import (
	"context"
	"fmt"
	"log"

	"Market-bot/internal/api"
)

func main() {
	client := api.NewRestClient("")
	event, err := client.GetEventByTokenID(context.Background(), "64903093311385616430821497488306433314807585397286521531639186532059591846310")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Event: %s\n", event.Slug)
}
