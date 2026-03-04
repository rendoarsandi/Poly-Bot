package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
)

func main() {
	// 1. Get an active market token ID
	resp, _ := http.Get("https://clob.polymarket.com/markets?limit=10&active=true&closed=false")
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)

	var data map[string]interface{}
	json.Unmarshal(body, &data)

	markets := data["data"].([]interface{})
	for _, m := range markets {
		market := m.(map[string]interface{})
		tokens := market["tokens"].([]interface{})
		for _, t := range tokens {
			token := t.(map[string]interface{})
			tokenID := token["token_id"].(string)

			// 2. Fetch the orderbook for this token
			respBook, _ := http.Get("https://clob.polymarket.com/book?token_id=" + tokenID)
			defer respBook.Body.Close()
			bookBody, _ := ioutil.ReadAll(respBook.Body)

			var book map[string]interface{}
			json.Unmarshal(bookBody, &book)

			if book["bids"] != nil && book["asks"] != nil {
				bids := book["bids"].([]interface{})
				asks := book["asks"].([]interface{})
				if len(bids) > 0 && len(asks) > 0 {
					fmt.Printf("Token ID: %s (%v)\n", tokenID, token["outcome"])
					fmt.Printf("Bids (top 2): %v\n", bids[:min(2, len(bids))])
					fmt.Printf("Asks (top 2): %v\n", asks[:min(2, len(asks))])
					return
				}
			}
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
