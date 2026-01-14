package main

import (
	"fmt"
	"time"
)

func main() {
	now := time.Now().UTC().Unix()
	window := (now / 900) * 900
	fmt.Printf("btc-updown-15m-%d\n", window)
}
