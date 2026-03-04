package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
)

func main() {
	resp, err := http.Get("https://gamma-api.polymarket.com/events?closed=false&limit=10")
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)

	var events []map[string]interface{}
	json.Unmarshal(body, &events)

	for _, event := range events {
		markets := event["markets"].([]interface{})
		for _, m := range markets {
			market := m.(map[string]interface{})
			clobTokenIds := market["clobTokenIds"].(string)
			var tokens []string
			json.Unmarshal([]byte(clobTokenIds), &tokens)
			if len(tokens) > 0 {
				tokenID := tokens[0]
				
				respBook, _ := http.Get("https://clob.polymarket.com/book?token_id=" + tokenID)
				defer respBook.Body.Close()
				bookBody, _ := ioutil.ReadAll(respBook.Body)
				
				var book map[string]interface{}
				json.Unmarshal(bookBody, &book)
				
				bids := book["bids"].([]interface{})
				asks := book["asks"].([]interface{})
				
				if len(bids) > 0 && len(asks) > 0 {
					fmt.Printf("Token ID: %s\n", tokenID)
					fmt.Printf("Bids[0]: %v\n", bids[0])
					fmt.Printf("Asks[0]: %v\n", asks[0])
					return
				}
			}
		}
	}
}
