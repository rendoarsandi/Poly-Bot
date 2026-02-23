package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run scripts/benchmark_rpc.go <RPC_URL>")
		return
	}

	rpcURL := os.Args[1]
	fmt.Printf("Testing RPC: %s\n", rpcURL)
	fmt.Println("--------------------------------------------------")

	var totalDuration time.Duration
	var minDuration = 10 * time.Second
	var maxDuration time.Duration
	iterations := 5 // Kurangi iterasi agar lebih cepat
	successCount := 0

	for i := 1; i <= iterations; i++ {
		start := time.Now()
		
		payload := map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "eth_blockNumber",
			"params":  []interface{}{},
			"id":      i,
		}
		body, _ := json.Marshal(payload)
		
		req, _ := http.NewRequestWithContext(context.Background(), "POST", rpcURL, bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		
		if err != nil {
			fmt.Printf("Iteration %d: FAILED (%v)\n", i, err)
			continue
		}
		resp.Body.Close()
		
		duration := time.Since(start)
		successCount++
		totalDuration += duration
		if duration < minDuration {
			minDuration = duration
		}
		if duration > maxDuration {
			maxDuration = duration
		}
		
		fmt.Printf("Iteration %d: %v\n", i, duration.Truncate(time.Millisecond))
		time.Sleep(100 * time.Millisecond)
	}

	if successCount > 0 {
		avg := totalDuration / time.Duration(successCount)
		fmt.Println("--------------------------------------------------")
		fmt.Printf("Average: %v\n", avg.Truncate(time.Millisecond))
		fmt.Printf("Fastest: %v\n", minDuration.Truncate(time.Millisecond))
		fmt.Printf("Slowest: %v\n", maxDuration.Truncate(time.Millisecond))
	}
}
