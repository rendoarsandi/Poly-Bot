package paper

import (
	"testing"
)

func TestTUI_RegisterSplitInventory(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	inv1 := NewSplitInventory()
	inv2 := NewSplitInventory()

	// Register inventories
	tui.RegisterSplitInventory(inv1)
	tui.RegisterSplitInventory(inv2)

	// Verify they were registered
	if len(tui.splitInventories) != 2 {
		t.Errorf("Expected 2 split inventories, got %d", len(tui.splitInventories))
	}
}

func TestTUI_SetWalletTruthPositions(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	tui.SetWalletTruthPositions("SOL", []WalletTruthPosition{{
		MarketID:      "SOL",
		Outcome:       "Up",
		LocalShares:   0.9311,
		OnChainShares: 0.9311,
		Drift:         0,
	}})

	positions := tui.getWalletTruthPositions()
	if len(positions) != 1 {
		t.Fatalf("expected 1 wallet truth position, got %d", len(positions))
	}
	if positions[0].MarketID != "SOL" || positions[0].Outcome != "Up" {
		t.Fatalf("unexpected wallet truth position: %+v", positions[0])
	}

	tui.ClearWalletTruthPositions("SOL")
	if got := tui.getWalletTruthPositions(); len(got) != 0 {
		t.Fatalf("expected wallet truth positions to clear, got %d", len(got))
	}
}

func TestTUI_SetWalletTruthPositionsClonesInput(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	positions := []WalletTruthPosition{{MarketID: "BTC", Outcome: "Yes", LocalShares: 1, OnChainShares: 1.25, Drift: 0.25}}
	tui.SetWalletTruthPositions("BTC", positions)
	positions[0].OnChainShares = 99

	got := tui.getWalletTruthPositions()
	if len(got) != 1 {
		t.Fatalf("expected 1 wallet truth position, got %d", len(got))
	}
	if got[0].OnChainShares != 1.25 {
		t.Fatalf("expected stored snapshot to be cloned, got %+v", got[0])
	}
}

func TestTUI_SetWalletTruthPositionsMarksDirty(t *testing.T) {
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	startVersion := tui.snapshotVersion

	tui.SetWalletTruthPositions("BTC", []WalletTruthPosition{{
		MarketID:      "BTC",
		Outcome:       "Down",
		LocalShares:   2.5,
		OnChainShares: 2.5,
	}})
	if tui.snapshotVersion <= startVersion {
		t.Fatalf("expected wallet-truth set to mark snapshot dirty, version %d -> %d", startVersion, tui.snapshotVersion)
	}

	midVersion := tui.snapshotVersion
	tui.ClearWalletTruthPositions("BTC")
	if tui.snapshotVersion <= midVersion {
		t.Fatalf("expected wallet-truth clear to mark snapshot dirty, version %d -> %d", midVersion, tui.snapshotVersion)
	}
}

func TestTUI_SetWalletTruthPositionsPreservesResolutionStateAcrossRefresh(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	tui.SetWalletTruthPositions("BTC#1", []WalletTruthPosition{{
		MarketID:         "BTC#1",
		Outcome:          "Up",
		LocalShares:      3.25,
		OnChainShares:    3.25,
		IsWinner:         true,
		Redeemable:       true,
		ResolutionStatus: "redeemable",
	}})

	tui.SetWalletTruthPositions("BTC#1", []WalletTruthPosition{{
		MarketID:      "BTC#1",
		Outcome:       "Up",
		LocalShares:   3.25,
		OnChainShares: 3.25,
	}})

	got := tui.getWalletTruthPositions()
	if len(got) != 1 {
		t.Fatalf("expected 1 wallet truth position, got %d", len(got))
	}
	if !got[0].IsWinner || !got[0].Redeemable || got[0].ResolutionStatus != "redeemable" {
		t.Fatalf("expected refresh to preserve resolution state, got %+v", got[0])
	}
}

func TestTUI_UpdateWalletTruthResolutionMatchesTrimmedWinner(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	tui.SetWalletTruthPositions("BTC#2", []WalletTruthPosition{
		{MarketID: "BTC#2", Outcome: "Up", OnChainShares: 3.25},
		{MarketID: "BTC#2", Outcome: "Down", OnChainShares: 0},
	})

	tui.UpdateWalletTruthResolution("BTC#2", true, " up ")

	got := tui.getWalletTruthPositions()
	if len(got) != 2 {
		t.Fatalf("expected 2 wallet truth positions, got %d", len(got))
	}
	for _, pos := range got {
		switch pos.Outcome {
		case "Up":
			if !pos.IsWinner || !pos.Redeemable || pos.ResolutionStatus != "redeemable" {
				t.Fatalf("expected Up to be recognized as winner, got %+v", pos)
			}
		case "Down":
			if pos.IsWinner || pos.Redeemable || pos.ResolutionStatus != "resolved" {
				t.Fatalf("expected Down to be resolved loser, got %+v", pos)
			}
		}
	}
}

func TestTUI_getSplitPositions(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	// Initially should be empty
	positions := tui.getSplitPositions()
	if len(positions) != 0 {
		t.Errorf("Expected 0 positions initially, got %d", len(positions))
	}

	// Create and register an inventory with positions
	inv := NewSplitInventory()
	inv.RecordSplit("BTC", "Up", "Down", 50.0)
	inv.RecordSplit("ETH", "Yes", "No", 30.0)

	tui.RegisterSplitInventory(inv)

	// Get positions
	positions = tui.getSplitPositions()

	// Should have 4 positions (2 markets x 2 outcomes)
	if len(positions) != 4 {
		t.Errorf("Expected 4 positions, got %d", len(positions))
	}

	// Verify positions contain expected data
	posMap := make(map[string]float64)
	for _, p := range positions {
		key := p.MarketID + ":" + p.Outcome
		posMap[key] = p.Shares
	}

	if shares, ok := posMap["BTC:Up"]; !ok || shares != 50.0 {
		t.Errorf("Expected BTC:Up = 50 shares, got %v", shares)
	}
	if shares, ok := posMap["BTC:Down"]; !ok || shares != 50.0 {
		t.Errorf("Expected BTC:Down = 50 shares, got %v", shares)
	}
	if shares, ok := posMap["ETH:Yes"]; !ok || shares != 30.0 {
		t.Errorf("Expected ETH:Yes = 30 shares, got %v", shares)
	}
	if shares, ok := posMap["ETH:No"]; !ok || shares != 30.0 {
		t.Errorf("Expected ETH:No = 30 shares, got %v", shares)
	}
}

func TestTUI_getSplitPositions_MultipleInventories(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	// Create multiple inventories
	inv1 := NewSplitInventory()
	inv1.RecordSplit("BTC", "Up", "Down", 50.0)

	inv2 := NewSplitInventory()
	inv2.RecordSplit("ETH", "Yes", "No", 30.0)

	tui.RegisterSplitInventory(inv1)
	tui.RegisterSplitInventory(inv2)

	// Get positions from all inventories
	positions := tui.getSplitPositions()

	// Should have 4 positions total
	if len(positions) != 4 {
		t.Errorf("Expected 4 positions from 2 inventories, got %d", len(positions))
	}

	// Verify all markets are represented
	marketCount := make(map[string]int)
	for _, p := range positions {
		marketCount[p.MarketID]++
	}

	if marketCount["BTC"] != 2 {
		t.Errorf("Expected 2 BTC positions (Up/Down), got %d", marketCount["BTC"])
	}
	if marketCount["ETH"] != 2 {
		t.Errorf("Expected 2 ETH positions (Yes/No), got %d", marketCount["ETH"])
	}
}

func TestTUI_getSplitPositions_ConcurrentAccess(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	inv := NewSplitInventory()
	inv.RecordSplit("BTC", "Up", "Down", 100.0)
	tui.RegisterSplitInventory(inv)

	// Test concurrent access - should not deadlock
	done := make(chan bool, 2)

	// Goroutine 1: repeatedly get positions
	go func() {
		for i := 0; i < 100; i++ {
			_ = tui.getSplitPositions()
		}
		done <- true
	}()

	// Goroutine 2: modify inventory
	go func() {
		for i := 0; i < 100; i++ {
			inv.RecordSell("BTC", "Up", 0.5, 0.55)
		}
		done <- true
	}()

	// Wait for both to complete
	<-done
	<-done

	// Final position check
	positions := tui.getSplitPositions()
	if len(positions) == 0 {
		t.Error("Expected positions after concurrent access")
	}
}

func TestTUI_RegisterSplitInventory_ThreadSafe(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	// Concurrent registration
	done := make(chan bool, 3)

	for i := 0; i < 3; i++ {
		go func() {
			inv := NewSplitInventory()
			inv.RecordSplit("BTC", "Up", "Down", 10.0)
			tui.RegisterSplitInventory(inv)
			done <- true
		}()
	}

	// Wait for all registrations
	<-done
	<-done
	<-done

	// Verify all inventories were registered
	if len(tui.splitInventories) != 3 {
		t.Errorf("Expected 3 split inventories, got %d", len(tui.splitInventories))
	}
}

func TestRecordWalletSyncAdjustmentAddsSyncHistoryEntry(t *testing.T) {
	t.Skip("Silenced ADJ logs per user request")
	tui := NewTUI(NewEngine(1000.0), NewOrderBook())
	tui.RecordWalletSyncAdjustment("BTC", "Down", 3.001719, 0.28, "ADJ+")

	history := tui.GetOrderHistory()
	if len(history) != 1 {
		t.Fatalf("expected 1 sync history entry, got %d", len(history))
	}
	if history[0].ExecutionMode != "wallet-sync" {
		t.Fatalf("expected wallet-sync execution mode, got %q", history[0].ExecutionMode)
	}
	if history[0].Status != "SYNCED" {
		t.Fatalf("expected SYNCED status, got %q", history[0].Status)
	}
	if history[0].Side != "ADJ+" {
		t.Fatalf("expected ADJ+ side, got %q", history[0].Side)
	}
}

func TestTUI_UpdateWalletTruthResolutionSplitTie(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	tui.SetWalletTruthPositions("BTC#3", []WalletTruthPosition{
		{MarketID: "BTC#3", Outcome: "Up", OnChainShares: 5.0},
		{MarketID: "BTC#3", Outcome: "Down", OnChainShares: 5.0},
	})

	tui.UpdateWalletTruthResolution("BTC#3", true, "Up/Down")

	got := tui.getWalletTruthPositions()
	if len(got) != 2 {
		t.Fatalf("expected 2 wallet truth positions, got %d", len(got))
	}
	for _, pos := range got {
		if !pos.IsWinner || !pos.Redeemable || pos.ResolutionStatus != "redeemable" {
			t.Fatalf("expected both Up and Down to be recognized as winners in a tie, got %+v", pos)
		}
	}
}

func TestTUI_UpdateWalletTruthRedeemableSplitTie(t *testing.T) {
	engine := NewEngine(1000.0)
	orderBook := NewOrderBook()
	tui := NewTUI(engine, orderBook)

	tui.SetWalletTruthPositions("BTC#4", []WalletTruthPosition{
		{MarketID: "BTC#4", Outcome: "Up", OnChainShares: 2.5},
		{MarketID: "BTC#4", Outcome: "Down", OnChainShares: 0},
	})

	tui.UpdateWalletTruthRedeemable("BTC#4", "Up/Down")

	got := tui.getWalletTruthPositions()
	if len(got) != 2 {
		t.Fatalf("expected 2 wallet truth positions, got %d", len(got))
	}
	for _, pos := range got {
		switch pos.Outcome {
		case "Up":
			if !pos.IsWinner || !pos.Redeemable || pos.ResolutionStatus != "redeemable" {
				t.Fatalf("expected Up to be winning/redeemable, got %+v", pos)
			}
		case "Down":
			if !pos.IsWinner || pos.Redeemable || pos.ResolutionStatus != "resolved" {
				t.Fatalf("expected Down to be resolved winner but not redeemable, got %+v", pos)
			}
		}
	}
}
