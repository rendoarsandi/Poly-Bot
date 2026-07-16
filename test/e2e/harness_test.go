package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"Market-bot/internal/api"
	"Market-bot/internal/paper"
)

// RPCRequest represents a JSON-RPC request format.
type RPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      interface{}   `json:"id"`
}

// RPCResponse represents a JSON-RPC response format.
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	ID      interface{}     `json:"id"`
}

// RPCError represents a JSON-RPC error format.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MockRPCServer simulates an Ethereum RPC endpoint with state/error injection.
type MockRPCServer struct {
	server           *httptest.Server
	healthy          bool
	mu               sync.Mutex
	GasPrice         *big.Int
	BaseFee          *big.Int
	PriorityFee      *big.Int
	Nonces           map[string]uint64
	Balances         map[string]*big.Int
	Allowances       map[string]map[string]*big.Int
	ResolvedMarkets  map[string]bool
	WinningOutcomes  map[string]string
	PayoutNumerators map[string]map[int]*big.Int
	CtfApproved      map[string]bool
	TxStatus         map[string]string // txHash -> status: "0x1" or "0x0"
	MethodCallCount  map[string]int
	Delay            time.Duration
	ForceError       string // error message to return for any request if set
}

// NewMockRPCServer instantiates a mock RPC server on a random local port.
func NewMockRPCServer() *MockRPCServer {
	s := &MockRPCServer{
		healthy:          true,
		GasPrice:         big.NewInt(50_000_000_000), // 50 gwei
		BaseFee:          big.NewInt(40_000_000_000),  // 40 gwei
		PriorityFee:      big.NewInt(2_000_000_000),   // 2 gwei
		Nonces:           make(map[string]uint64),
		Balances:         make(map[string]*big.Int),
		Allowances:       make(map[string]map[string]*big.Int),
		ResolvedMarkets:  make(map[string]bool),
		WinningOutcomes:  make(map[string]string),
		PayoutNumerators: make(map[string]map[int]*big.Int),
		CtfApproved:      make(map[string]bool),
		TxStatus:         make(map[string]string),
		MethodCallCount:  make(map[string]int),
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handleRPC))
	return s
}

// URL returns the local http address of the mock server.
func (s *MockRPCServer) URL() string {
	return s.server.URL
}

// Close shuts down the server.
func (s *MockRPCServer) Close() {
	s.server.Close()
}

// SetHealthy toggles the server's availability.
func (s *MockRPCServer) SetHealthy(healthy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthy = healthy
}

// SetBalanceFloat sets a user's collateral balance using human-readable floats (6 decimals).
func (s *MockRPCServer) SetBalanceFloat(addr string, val float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bi := new(big.Float).Mul(big.NewFloat(val), big.NewFloat(1e6))
	biInt, _ := bi.Int(nil)
	s.Balances[strings.ToLower(addr)] = biInt
}

func (s *MockRPCServer) handleRPC(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	healthy := s.healthy
	delay := s.Delay
	forceErr := s.ForceError
	s.mu.Unlock()

	if !healthy {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if delay > 0 {
		time.Sleep(delay)
	}

	var req RPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.MethodCallCount[req.Method]++
	s.mu.Unlock()

	var resp RPCResponse
	resp.JSONRPC = "2.0"
	resp.ID = req.ID

	if forceErr != "" {
		resp.Error = &RPCError{Code: -32000, Message: forceErr}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var result interface{}
	var err error

	switch req.Method {
	case "eth_gasPrice":
		result = fmt.Sprintf("0x%x", s.GasPrice)
	case "eth_maxPriorityFeePerGas":
		result = fmt.Sprintf("0x%x", s.PriorityFee)
	case "eth_getBlockByNumber":
		result = map[string]interface{}{
			"baseFeePerGas": fmt.Sprintf("0x%x", s.BaseFee),
			"number":        "0x1",
		}
	case "eth_getTransactionCount":
		if len(req.Params) > 0 {
			addr, _ := req.Params[0].(string)
			nonce := s.Nonces[strings.ToLower(addr)]
			result = fmt.Sprintf("0x%x", nonce)
		} else {
			result = "0x0"
		}
	case "eth_call":
		if len(req.Params) > 0 {
			callObj, ok := req.Params[0].(map[string]interface{})
			if ok {
				to, _ := callObj["to"].(string)
				data, _ := callObj["data"].(string)
				result, err = s.handleEthCall(to, data)
			}
		}
	case "eth_sendRawTransaction":
		result = "0x" + strings.Repeat("a", 64)
	case "eth_getTransactionReceipt":
		if len(req.Params) > 0 {
			txHash, _ := req.Params[0].(string)
			status, exists := s.TxStatus[txHash]
			if !exists {
				status = "0x1"
			}
			result = map[string]interface{}{
				"status":          status,
				"blockNumber":     "0x1",
				"gasUsed":         "0x15f90",
				"transactionHash": txHash,
				"logs":            []interface{}{},
			}
		}
	case "eth_getTransactionByHash":
		if len(req.Params) > 0 {
			txHash, _ := req.Params[0].(string)
			result = map[string]interface{}{
				"hash":        txHash,
				"blockNumber": "0x1",
			}
		}
	default:
		resp.Error = &RPCError{Code: -32601, Message: "Method not found"}
	}

	if resp.Error == nil {
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else {
			resBytes, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				resp.Error = &RPCError{Code: -32603, Message: marshalErr.Error()}
			} else {
				resp.Result = resBytes
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *MockRPCServer) handleEthCall(to, data string) (string, error) {
	data = strings.TrimPrefix(data, "0x")
	to = strings.ToLower(to)

	switch {
	case strings.HasPrefix(data, "70a08231"): // balanceOf(address)
		if len(data) >= 8+64 {
			addrHex := data[8:]
			addr := "0x" + strings.ToLower(strings.TrimLeft(addrHex, "0"))
			if addr == "0x" {
				addr = "0x0"
			}
			val := s.Balances[addr]
			if val == nil {
				val = big.NewInt(0)
			}
			return fmt.Sprintf("0x%064x", val), nil
		}
	case strings.HasPrefix(data, "dd62ed3e"): // allowance(address,address)
		if len(data) >= 8+128 {
			owner := "0x" + strings.ToLower(strings.TrimLeft(data[8:8+64], "0"))
			spender := "0x" + strings.ToLower(strings.TrimLeft(data[8+64:], "0"))
			if owner == "0x" {
				owner = "0x0"
			}
			if spender == "0x" {
				spender = "0x0"
			}
			allowance := big.NewInt(0)
			if s.Allowances[owner] != nil {
				if val, ok := s.Allowances[owner][spender]; ok {
					allowance = val
				}
			}
			return fmt.Sprintf("0x%064x", allowance), nil
		}
	case strings.HasPrefix(data, "e985e9c5"): // isApprovedForAll(address,address)
		if len(data) >= 8+128 {
			owner := "0x" + strings.ToLower(strings.TrimLeft(data[8:8+64], "0"))
			operator := "0x" + strings.ToLower(strings.TrimLeft(data[8+64:], "0"))
			if owner == "0x" {
				owner = "0x0"
			}
			if operator == "0x" {
				operator = "0x0"
			}
			approved := s.CtfApproved[owner+":"+operator]
			val := int64(0)
			if approved {
				val = 1
			}
			return fmt.Sprintf("0x%064x", val), nil
		}
	case strings.HasPrefix(data, "dd34de67"): // payoutDenominator(bytes32)
		if len(data) >= 8+64 {
			condID := "0x" + strings.ToLower(data[8:8+64])
			resolved := s.ResolvedMarkets[condID]
			denom := big.NewInt(0)
			if resolved {
				denom = big.NewInt(1)
			}
			return fmt.Sprintf("0x%064x", denom), nil
		}
	case strings.HasPrefix(data, "0504c814"): // payoutNumerators(bytes32,uint256)
		if len(data) >= 8+128 {
			condID := "0x" + strings.ToLower(data[8:8+64])
			idxHex := strings.TrimLeft(data[8+64:], "0")
			if idxHex == "" {
				idxHex = "0"
			}
			idxVal, _ := strconv.ParseInt(idxHex, 16, 64)
			val := big.NewInt(0)
			if s.PayoutNumerators[condID] != nil {
				if num, ok := s.PayoutNumerators[condID][int(idxVal)]; ok {
					val = num
				}
			}
			return fmt.Sprintf("0x%064x", val), nil
		}
	}

	return fmt.Sprintf("0x%064x", 0), nil
}

// FallbackClient acts as a client wrapper to test fallback endpoint policies.
type FallbackClient struct {
	mu         sync.Mutex
	endpoints  []string
	activeIdx  int
	clients    []*api.PolygonClient
	maxRetries int
}

func NewFallbackClient(endpoints []string) *FallbackClient {
	clients := make([]*api.PolygonClient, len(endpoints))
	for i, url := range endpoints {
		clients[i] = api.NewPolygonClient(url)
	}
	return &FallbackClient{
		endpoints:  endpoints,
		clients:    clients,
		maxRetries: len(endpoints),
	}
}

func (fc *FallbackClient) ActiveClient() *api.PolygonClient {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.clients[fc.activeIdx]
}

func (fc *FallbackClient) Failover() {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.activeIdx = (fc.activeIdx + 1) % len(fc.endpoints)
}

func (fc *FallbackClient) CallWithFallback(action func(client *api.PolygonClient) error) error {
	var lastErr error
	for i := 0; i < fc.maxRetries; i++ {
		client := fc.ActiveClient()
		err := action(client)
		if err == nil {
			return nil
		}
		lastErr = err
		fc.Failover()
	}
	return fmt.Errorf("all endpoints failed, last error: %w", lastErr)
}

// MockBinanceWSServer handles real WebSocket connections, pushing mock trade/mark ticks to price feeds.
type MockBinanceWSServer struct {
	server  *httptest.Server
	clients map[*websocket.Conn]bool
	mu      sync.Mutex
}

func NewMockBinanceWSServer() *MockBinanceWSServer {
	m := &MockBinanceWSServer{
		clients: make(map[*websocket.Conn]bool),
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.handler))
	return m
}

func (m *MockBinanceWSServer) URL() string {
	return "ws" + m.server.URL[4:]
}

func (m *MockBinanceWSServer) Close() {
	m.server.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	for c := range m.clients {
		_ = c.Close(websocket.StatusNormalClosure, "shutdown")
	}
}

func (m *MockBinanceWSServer) handler(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	m.mu.Lock()
	m.clients[c] = true
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.clients, c)
		m.mu.Unlock()
		_ = c.Close(websocket.StatusInternalError, "connection closed")
	}()

	ctx := r.Context()
	for {
		_, _, err := c.Read(ctx)
		if err != nil {
			break
		}
	}
}

func (m *MockBinanceWSServer) PushTrade(symbol string, price float64, tradeTime time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	msg := map[string]interface{}{
		"stream": strings.ToLower(symbol) + "@aggTrade",
		"data": map[string]interface{}{
			"e": "aggTrade",
			"s": symbol,
			"p": fmt.Sprintf("%.2f", price),
			"T": tradeTime.UnixMilli(),
			"E": tradeTime.UnixMilli(),
		},
	}

	for c := range m.clients {
		_ = wsjson.Write(context.Background(), c, msg)
	}
}

func (m *MockBinanceWSServer) PushMarkPrice(symbol string, price float64, eventTime time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	msg := map[string]interface{}{
		"stream": strings.ToLower(symbol) + "@markPrice@1s",
		"data": map[string]interface{}{
			"e": "markPriceUpdate",
			"p": fmt.Sprintf("%.2f", price),
			"E": eventTime.UnixMilli(),
		},
	}

	for c := range m.clients {
		_ = wsjson.Write(context.Background(), c, msg)
	}
}

// MockPaperEngine stubs the paper engine execution model, trade tracking, and inventory skew.
type MockPaperEngine struct {
	Engine *paper.Engine
}

func NewMockPaperEngine(startingBalance float64) *MockPaperEngine {
	return &MockPaperEngine{
		Engine: paper.NewEngine(startingBalance),
	}
}

func (m *MockPaperEngine) SetPosition(marketID, outcome string, qty, markPrice float64) {
	m.Engine.SyncExternalPosition(marketID, outcome, qty, markPrice)
}

func (m *MockPaperEngine) SetPositionWithTotalCost(marketID, outcome string, qty, totalCost float64) {
	m.Engine.SyncExternalPositionWithTotalCost(marketID, outcome, qty, totalCost)
}

func (m *MockPaperEngine) SimulateTrade(marketID, outcome, side string, price, quantity float64) (*paper.Trade, error) {
	if strings.ToLower(side) == "buy" {
		return m.Engine.BuyForMarket(marketID, outcome, price, quantity)
	}
	return m.Engine.SellForMarket(marketID, outcome, price, quantity)
}

func (m *MockPaperEngine) GetSkew(marketID string, yesOutcome, noOutcome string) float64 {
	// Skew = YES quantity - NO quantity
	yesQty := 0.0
	noQty := 0.0

	positions := m.Engine.GetPositions()
	for _, p := range positions {
		if p.MarketID == marketID {
			if strings.EqualFold(p.Outcome, yesOutcome) {
				yesQty = p.Quantity
			} else if strings.EqualFold(p.Outcome, noOutcome) {
				noQty = p.Quantity
			}
		}
	}
	return yesQty - noQty
}

func (m *MockPaperEngine) Reset(balance float64) error {
	return m.Engine.ResetPaperSession(balance)
}

// MockWidgetRenderer formats text elements simulating actual TUI panel outputs to verify UI.
type MockWidgetRenderer struct {
	width  int
	height int
}

func NewMockWidgetRenderer(width, height int) *MockWidgetRenderer {
	return &MockWidgetRenderer{
		width:  width,
		height: height,
	}
}

func (r *MockWidgetRenderer) RenderMarketGrid(marketID string, outcomes []string, bids, asks map[string]float64, source, status string, signalSymbol string, signalPrice, signalDelta float64, signalTarget string) string {
	var sb strings.Builder
	sb.WriteString("┌" + strings.Repeat("─", r.width-2) + "┐\n")
	sb.WriteString(fmt.Sprintf("│ MARKET: %-*s │\n", r.width-12, marketID))

	outcomesStr := ""
	for i, o := range outcomes {
		outcomesStr += fmt.Sprintf("%s ($%.2f / $%.2f)", o, bids[o], asks[o])
		if i < len(outcomes)-1 {
			outcomesStr += " | "
		}
	}
	sb.WriteString(fmt.Sprintf("│ Outcomes: %-*s │\n", r.width-14, outcomesStr))
	sb.WriteString(fmt.Sprintf("│ Source: %-10s | Status: %-*s │\n", source, r.width-25, status))
	if signalSymbol != "" {
		sb.WriteString(fmt.Sprintf("│ Binance: %s $%.2f | Delta: %.2f%% (Target: %s) %-*s │\n",
			signalSymbol, signalPrice, signalDelta, signalTarget, r.width-50, ""))
	}
	sb.WriteString("└" + strings.Repeat("─", r.width-2) + "┘")
	return sb.String()
}

func (r *MockWidgetRenderer) RenderSettingsPanel(exchange, backend string, maxMarkets int, sizingMode string, scaleFactor float64, minMargin float64) string {
	var sb strings.Builder
	sb.WriteString("┌" + strings.Repeat("─", r.width-2) + "┐\n")
	sb.WriteString(fmt.Sprintf("│ SETTINGS: Exchange=%-10s | Backend=%-10s %-*s │\n", exchange, backend, r.width-44, ""))
	sb.WriteString(fmt.Sprintf("│ Max Markets: %-5d | Sizing: %-7s (%.2f%%) %-*s │\n", maxMarkets, sizingMode, scaleFactor*100.0, r.width-42, ""))
	sb.WriteString(fmt.Sprintf("│ Min Margin: %.2f%% %-*s │\n", minMargin, r.width-21, ""))
	sb.WriteString("└" + strings.Repeat("─", r.width-2) + "┘")
	return sb.String()
}

func (r *MockWidgetRenderer) RenderLogViewer(logs []string) string {
	var sb strings.Builder
	sb.WriteString("┌" + strings.Repeat("─", r.width-2) + "┐\n")
	sb.WriteString(fmt.Sprintf("│ EVENT LOGS: %-*s │\n", r.width-16, ""))
	for i, log := range logs {
		if i >= r.height-4 {
			break
		}
		if len(log) > r.width-6 {
			log = log[:r.width-9] + "..."
		}
		sb.WriteString(fmt.Sprintf("│ %-*s │\n", r.width-4, log))
	}
	sb.WriteString("└" + strings.Repeat("─", r.width-2) + "┘")
	return sb.String()
}

// ─── HARNESS TESTS ────────────────────────────────────────────────────────────

func TestMockRPCServer(t *testing.T) {
	s := NewMockRPCServer()
	defer s.Close()

	s.SetBalanceFloat("0x1234567890123456789012345678901234567890", 1500.50)

	ctx := context.Background()
	client := api.NewPolygonClient(s.URL())

	// Test gas price retrieval
	gas, err := client.GetGasPrice(ctx)
	if err != nil {
		t.Fatalf("failed to get gas price: %v", err)
	}
	if gas.Cmp(big.NewInt(50_000_000_000)) != 0 {
		t.Errorf("expected gas price 50 gwei, got %v", gas)
	}

	// Test base fee retrieval
	baseFee, err := client.GetBlockBaseFee(ctx)
	if err != nil {
		t.Fatalf("failed to get base fee: %v", err)
	}
	if baseFee.Cmp(big.NewInt(40_000_000_000)) != 0 {
		t.Errorf("expected base fee 40 gwei, got %v", baseFee)
	}

	// Test balance retrieval
	balance, err := client.GetCollateralBalance(ctx, "0x1234567890123456789012345678901234567890")
	if err != nil {
		t.Fatalf("failed to get balance: %v", err)
	}
	if balance != 1500.50 {
		t.Errorf("expected balance 1500.50, got %.2f", balance)
	}

	// Verify call counting
	s.mu.Lock()
	count := s.MethodCallCount["eth_gasPrice"]
	s.mu.Unlock()
	if count != 1 {
		t.Errorf("expected 1 call count for eth_gasPrice, got %d", count)
	}
}

func TestFallbackClient(t *testing.T) {
	s1 := NewMockRPCServer()
	defer s1.Close()
	s2 := NewMockRPCServer()
	defer s2.Close()

	// Make s1 unhealthy to trigger fallback to s2
	s1.SetHealthy(false)

	fc := NewFallbackClient([]string{s1.URL(), s2.URL()})

	ctx := context.Background()
	var gas *big.Int
	err := fc.CallWithFallback(func(client *api.PolygonClient) error {
		var callErr error
		gas, callErr = client.GetGasPrice(ctx)
		return callErr
	})

	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if gas.Cmp(big.NewInt(50_000_000_000)) != 0 {
		t.Errorf("unexpected gas price: %v", gas)
	}

	// Ensure s1 was called and failed, and s2 succeeded
	s1.mu.Lock()
	s1Called := s1.MethodCallCount["eth_gasPrice"]
	s1.mu.Unlock()
	s2.mu.Lock()
	s2Called := s2.MethodCallCount["eth_gasPrice"]
	s2.mu.Unlock()

	if s1Called != 0 {
		t.Errorf("s1 should not have processed eth_gasPrice since it returned 500 status")
	}
	if s2Called != 1 {
		t.Errorf("expected s2 to process eth_gasPrice, got %d", s2Called)
	}
}

func TestMockBinanceWSServer(t *testing.T) {
	wsSrv := NewMockBinanceWSServer()
	defer wsSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	feed := api.NewBinanceFuturesPriceFeed("BTCUSDT", 100*time.Millisecond)
	// Inject mock WS URL into feed
	feed.RecordTradeSampleForTest(1.0, time.Time{}) // initialize feed list
	
	// Start websocket server client connection manually or feed.Start
	// Wait, we can test that our WS server pushes properly
	c, _, err := websocket.Dial(ctx, wsSrv.URL(), nil)
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Listen in background
	tickCh := make(chan map[string]interface{}, 1)
	go func() {
		var raw map[string]interface{}
		if err := wsjson.Read(ctx, c, &raw); err == nil {
			tickCh <- raw
		}
	}()

	time.Sleep(10 * time.Millisecond)
	wsSrv.PushTrade("BTCUSDT", 84250.5, time.Now())

	select {
	case tick := <-tickCh:
		stream, _ := tick["stream"].(string)
		if stream != "btcusdt@aggTrade" {
			t.Errorf("unexpected stream: %q", stream)
		}
		data, _ := tick["data"].(map[string]interface{})
		pStr, _ := data["p"].(string)
		if pStr != "84250.50" {
			t.Errorf("unexpected price: %q", pStr)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for tick message")
	}
}

func TestMockPaperEngine(t *testing.T) {
	m := NewMockPaperEngine(5000.0)
	m.SetPosition("BTC", "Yes", 1000.0, 0.45)
	m.SetPosition("BTC", "No", 200.0, 0.55)

	skew := m.GetSkew("BTC", "Yes", "No")
	if skew != 800.0 {
		t.Errorf("expected skew 800, got %.2f", skew)
	}

	_, err := m.SimulateTrade("BTC", "Yes", "sell", 0.44, 100.0)
	if err != nil {
		t.Fatalf("failed to simulate sell trade: %v", err)
	}

	skew = m.GetSkew("BTC", "Yes", "No")
	if skew != 700.0 {
		t.Errorf("expected skew 700, got %.2f", skew)
	}
}

func TestMockWidgetRenderer(t *testing.T) {
	r := NewMockWidgetRenderer(80, 10)

	bids := map[string]float64{"Yes": 0.41, "No": 0.57}
	asks := map[string]float64{"Yes": 0.43, "No": 0.59}
	grid := r.RenderMarketGrid("BTC-15M", []string{"Yes", "No"}, bids, asks, "WS", "active", "BTCUSDT", 84250.5, 0.64, "Yes")

	if !strings.Contains(grid, "MARKET: BTC-15M") {
		t.Errorf("expected market ID in output, got %q", grid)
	}
	if !strings.Contains(grid, "Yes ($0.41 / $0.43)") {
		t.Errorf("expected outcome bids/asks in output, got %q", grid)
	}
	if !strings.Contains(grid, "Binance: BTCUSDT") {
		t.Errorf("expected Binance symbol in output, got %q", grid)
	}

	settings := r.RenderSettingsPanel("polymarket", "paper", 5, "percent", 0.05, 2.0)
	if !strings.Contains(settings, "Exchange=polymarket") {
		t.Errorf("expected Exchange in settings, got %q", settings)
	}
	if !strings.Contains(settings, "Max Markets: 5") {
		t.Errorf("expected Max Markets in settings, got %q", settings)
	}
}
