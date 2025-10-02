package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/gorilla/websocket"
)

// Скрипт: подписка на Uniswap V4 PoolManager (Ethereum) и вычисление цены для пулов UNI/USDC и LINK/USDC.
// Требуется: ALCHEMY_API_KEY или ALCHEMY_WS_URL, POOLMANAGER_V4 (адрес), путь к JSON с пулами (GECKO_POOLS_JSON).
// Прерывание: Ctrl+C или таймаут 10 минут.

const (
	defaultMainnetTemplateV4 = "wss://eth-mainnet.g.alchemy.com/v2/%s"
	poolManagerABIPath       = "ABI/Uniswap/V4/UniswapV4PoolManager.json"
	defaultTimeout           = 10 * time.Minute
)

type jsonRPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type subscriptionAck struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  string `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type subscriptionNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		Subscription string    `json:"subscription"`
		Result       logResult `json:"result"`
	} `json:"params"`
}

type logResult struct {
	Address          string   `json:"address"`
	BlockHash        string   `json:"blockHash"`
	BlockNumber      string   `json:"blockNumber"`
	Data             string   `json:"data"`
	LogIndex         string   `json:"logIndex"`
	Removed          bool     `json:"removed"`
	Topics           []string `json:"topics"`
	TransactionHash  string   `json:"transactionHash"`
	TransactionIndex string   `json:"transactionIndex"`
}

type PoolEntry struct {
	Dex      string `json:"dex"`
	PairName string `json:"pair_name"`
	PoolID   string `json:"pool_id"`
	PoolAddr string `json:"pool_address"`
	Network  string `json:"network"`
	Token0   struct {
		Address  string `json:"address"`
		Symbol   string `json:"symbol"`
		Decimals int    `json:"decimals"`
	} `json:"token0"`
	Token1 struct {
		Address  string `json:"address"`
		Symbol   string `json:"symbol"`
		Decimals int    `json:"decimals"`
	} `json:"token1"`
}

type GeckoFile struct {
	Entries []PoolEntry `json:"entries"`
}

type poolMeta struct {
	PoolID    common.Hash
	Symbol0   string
	Symbol1   string
	Addr0     common.Address
	Addr1     common.Address
	Decimals0 int
	Decimals1 int
	BaseIs0   bool // base = token0?
	PairName  string
	LastPrice *big.Rat
}

var (
	managerABI  abi.ABI
	eventBySig  map[common.Hash]abi.Event
	poolByID    = make(map[common.Hash]*poolMeta)
	wantedPairs = []string{"UNI/USDC", "LINK/USDC"}
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := loadABI(); err != nil {
		log.Fatalf("ABI load error: %v", err)
	}
	if err := loadPools(); err != nil {
		log.Fatalf("load pools error: %v", err)
	}
	if len(poolByID) == 0 {
		log.Fatalf("no target V4 pools found in JSON for %v", wantedPairs)
	}
	wsURL, err := resolveAlchemyWSURL()
	if err != nil {
		log.Fatalf("resolve ws url: %v", err)
	}
	pmAddr := strings.TrimSpace(os.Getenv("POOLMANAGER_V4"))
	if pmAddr == "" {
		log.Fatalf("set POOLMANAGER_V4 env (PoolManager address)")
	}
	if !common.IsHexAddress(pmAddr) {
		log.Fatalf("invalid PoolManager address: %s", pmAddr)
	}
	log.Printf("[INFO] Using PoolManager: %s", pmAddr)

	dialer := *websocket.DefaultDialer
	dialer.EnableCompression = true
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	log.Printf("[INFO] Connected WS %s", wsURL)

	subReq := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_subscribe",
		Params: []interface{}{
			"logs",
			map[string]interface{}{
				"address": []string{pmAddr},
			},
		},
	}
	if err := conn.WriteJSON(subReq); err != nil {
		log.Fatalf("subscribe write: %v", err)
	}
	payload, _ := json.Marshal(subReq)
	log.Printf("[INFO] Subscription sent: %s", string(payload))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pingLoop(ctx, conn)

	messages := make(chan []byte, 64)
	errCh := make(chan error, 1)
	go reader(conn, messages, errCh)

	sigCh := make(chan os.Signal, 1)
	osignalNotify(sigCh)
	endTimer := time.NewTimer(defaultTimeout)
	defer endTimer.Stop()

	for {
		select {
		case raw, ok := <-messages:
			if !ok {
				log.Printf("[INFO] reader closed")
				return
			}
			handleMessage(raw)
		case err := <-errCh:
			log.Printf("[ERR ] %v", err)
			return
		case <-sigCh:
			log.Printf("[INFO] interrupt, exit")
			return
		case <-endTimer.C:
			log.Printf("[INFO] timeout reached, exit")
			return
		}
	}
}

func loadABI() error {
	content, err := os.ReadFile(poolManagerABIPath)
	if err != nil {
		return err
	}
	parsed, err := abi.JSON(bytes.NewReader(content))
	if err != nil {
		return err
	}
	managerABI = parsed
	eventBySig = make(map[common.Hash]abi.Event)
	for _, e := range managerABI.Events {
		eventBySig[e.ID] = e
	}
	return nil
}

func loadPools() error {
	path := os.Getenv("GECKO_POOLS_JSON")
	if path == "" {
		path = "ticker_source/geckoterminal_pools.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var gf GeckoFile
	if err := json.Unmarshal(data, &gf); err != nil {
		return err
	}
	wanted := make(map[string]struct{})
	for _, p := range wantedPairs {
		wanted[p] = struct{}{}
	}
	count := 0
	for _, e := range gf.Entries {
		if !strings.EqualFold(e.Dex, "uniswap_v4") {
			continue
		}
		if _, ok := wanted[e.PairName]; !ok {
			continue
		}
		// Проверяем корректность pool_id (bytes32 hex)
		pid := strings.ToLower(strings.TrimSpace(e.PoolID))
		if !strings.HasPrefix(pid, "0x") || len(pid) != 66 {
			log.Printf("[WARN] skip pool %s invalid pool_id=%s", e.PairName, e.PoolID)
			continue
		}
		h := common.HexToHash(pid)
		pm := &poolMeta{
			PoolID:    h,
			Symbol0:   e.Token0.Symbol,
			Symbol1:   e.Token1.Symbol,
			Addr0:     common.HexToAddress(e.Token0.Address),
			Addr1:     common.HexToAddress(e.Token1.Address),
			Decimals0: e.Token0.Decimals,
			Decimals1: e.Token1.Decimals,
			BaseIs0:   strings.Contains(strings.ToUpper(e.PairName), e.Token0.Symbol) && strings.Contains(strings.ToUpper(e.PairName), "USDC") && !strings.Contains(strings.ToUpper(e.Token1.Symbol), "USDC"),
			PairName:  e.PairName,
		}
		// Простая эвристика: если второй токен USDC — считаем base = первый (UNI/USDC => base=UNI). Для LINK/USDC аналогично.
		if strings.HasSuffix(strings.ToUpper(e.PairName), "/USDC") {
			pm.BaseIs0 = true
		}
		poolByID[h] = pm
		count++
		log.Printf("[INFO] added pool %s id=%s token0=%s token1=%s", e.PairName, h.Hex(), e.Token0.Symbol, e.Token1.Symbol)
		if count == len(wantedPairs) {
			break
		}
	}
	return nil
}

func resolveAlchemyWSURL() (string, error) {
	if direct := strings.TrimSpace(os.Getenv("ALCHEMY_WS_URL")); direct != "" {
		return direct, nil
	}
	apiKey := strings.TrimSpace(os.Getenv("ALCHEMY_API_KEY"))
	if apiKey == "" {
		return "", fmt.Errorf("set ALCHEMY_WS_URL or ALCHEMY_API_KEY")
	}
	return fmt.Sprintf(defaultMainnetTemplateV4, apiKey), nil
}

func reader(conn *websocket.Conn, out chan<- []byte, errs chan<- error) {
	defer close(out)
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			errs <- err
			return
		}
		if mt == websocket.PongMessage {
			continue
		}
		out <- data
	}
}

func pingLoop(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(25 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = c.WriteMessage(websocket.PingMessage, nil)
		}
	}
}

func handleMessage(raw []byte) {
	if ack, ok := tryAck(raw); ok {
		if ack.Error != nil {
			log.Printf("[ERR ] subscribe: %d %s", ack.Error.Code, ack.Error.Message)
		} else {
			log.Printf("[INFO] subscribed id=%s", ack.Result)
		}
		return
	}
	note, ok := tryNote(raw)
	if !ok {
		return
	}
	processLog(note.Params.Result)
}

func tryAck(raw []byte) (*subscriptionAck, bool) {
	var a subscriptionAck
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, false
	}
	if a.Result == "" && a.Error == nil {
		return nil, false
	}
	return &a, true
}

func tryNote(raw []byte) (*subscriptionNotification, bool) {
	var n subscriptionNotification
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, false
	}
	if !strings.EqualFold(n.Method, "eth_subscription") {
		return nil, false
	}
	return &n, true
}

func processLog(lr logResult) {
	if lr.Removed {
		return
	}
	if len(lr.Topics) == 0 {
		return
	}
	sig := common.HexToHash(lr.Topics[0])
	evt, ok := eventBySig[sig]
	if !ok {
		return
	}
	if evt.Name != "Swap" {
		return
	} // интересуют только Swap
	// Swap indexed[0] = PoolId (bytes32)
	if len(lr.Topics) < 2 {
		return
	}
	poolID := common.HexToHash(lr.Topics[1])
	meta, okMeta := poolByID[poolID]
	if !okMeta {
		return
	}
	fields := make(map[string]interface{})
	// Unpack non-indexed из Data
	if lr.Data != "0x" {
		dataBytes, err := hexutil.Decode(lr.Data)
		if err != nil {
			log.Printf("[DECODE] hex data err: %v", err)
			return
		}
		if err := evt.Inputs.NonIndexed().UnpackIntoMap(fields, dataBytes); err != nil {
			log.Printf("[DECODE] unpack err: %v", err)
			return
		}
	}
	// Порядок в ABI для Swap non-indexed: amount0(int128), amount1(int128), sqrtPriceX96(uint160), liquidity(uint128), tick(int24), fee(uint24)
	amount0, ok0 := bigIntFrom(fields["amount0"])
	amount1, ok1 := bigIntFrom(fields["amount1"])
	sqrtPrice, ok2 := bigIntFrom(fields["sqrtPriceX96"])
	liquidity, _ := bigIntFrom(fields["liquidity"]) // инфо
	tick, _ := bigIntFrom(fields["tick"])           // инфо
	fee, _ := bigIntFrom(fields["fee"])             // инфо
	if !(ok0 && ok1 && ok2) {
		return
	}
	price := derivePrice(meta, amount0, amount1, sqrtPrice)
	publishPrice(meta, price, lr, amount0, amount1, sqrtPrice, liquidity, tick, fee)
}

func bigIntFrom(v interface{}) (*big.Int, bool) {
	switch x := v.(type) {
	case *big.Int:
		return x, true
	case int64:
		return big.NewInt(x), true
	case uint64:
		return new(big.Int).SetUint64(x), true
	case string:
		if strings.HasPrefix(x, "0x") {
			bi, err := hexutil.DecodeBig(x)
			if err == nil {
				return bi, true
			}
		}
	}
	return nil, false
}

var twoPow192 = new(big.Int).Lsh(big.NewInt(1), 192)

func derivePrice(meta *poolMeta, amount0, amount1, sqrtPrice *big.Int) *big.Rat {
	if sqrtPrice != nil && sqrtPrice.Sign() > 0 {
		// price token0/token1 = (sqrtP^2)/2^192
		sq := new(big.Int).Mul(sqrtPrice, sqrtPrice)
		raw := new(big.Rat).SetFrac(sq, twoPow192)
		adj := new(big.Rat).SetFrac(tenPow(uint(meta.Decimals0)), tenPow(uint(meta.Decimals1)))
		p0over1 := new(big.Rat).Mul(raw, adj)
		if meta.BaseIs0 {
			return p0over1
		}
		return new(big.Rat).Inv(p0over1)
	}
	// fallback amounts
	abs0 := absBig(amount0)
	abs1 := absBig(amount1)
	if meta.BaseIs0 {
		return ratio(abs1, meta.Decimals1, abs0, meta.Decimals0)
	}
	return ratio(abs0, meta.Decimals0, abs1, meta.Decimals1)
}

func ratio(num *big.Int, numDec int, den *big.Int, denDec int) *big.Rat {
	if den.Sign() == 0 {
		return nil
	}
	nr := new(big.Rat).SetFrac(num, tenPow(uint(numDec)))
	dr := new(big.Rat).SetFrac(den, tenPow(uint(denDec)))
	return new(big.Rat).Quo(nr, dr)
}

func tenPow(dec uint) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil) }
func absBig(v *big.Int) *big.Int {
	if v.Sign() < 0 {
		return new(big.Int).Neg(v)
	}
	return v
}

func publishPrice(meta *poolMeta, price *big.Rat, lr logResult, amount0, amount1, sqrtP, liq, tick, fee *big.Int) {
	if price == nil {
		return
	}
	priceStr := formatRat(price, 8)
	changed := meta.LastPrice == nil || new(big.Rat).Sub(price, meta.LastPrice).Abs(price).Cmp(new(big.Rat).Quo(meta.LastPrice, big.NewRat(10000, 1))) > 0 // ~> 1 bp threshold
	if changed {
		meta.LastPrice = new(big.Rat).Set(price)
		block := blockNumber(lr.BlockNumber)
		log.Printf("[SWAP] %s price=%s base=%s quote=%s sqrtP=%s amt0=%s amt1=%s liq=%s tick=%s fee=%s block=%d tx=%s", meta.PairName, priceStr, baseSym(meta), quoteSym(meta), safeBig(sqrtP), safeBig(amount0), safeBig(amount1), safeBig(liq), safeBig(tick), safeBig(fee), block, lr.TransactionHash)
	}
}

func baseSym(m *poolMeta) string {
	if m.BaseIs0 {
		return m.Symbol0
	}
	return m.Symbol1
}
func quoteSym(m *poolMeta) string {
	if m.BaseIs0 {
		return m.Symbol1
	}
	return m.Symbol0
}

func formatRat(r *big.Rat, prec int) string {
	if r == nil {
		return "?"
	}
	f := new(big.Float).SetPrec(256).SetRat(r)
	return f.Text('f', prec)
}

func blockNumber(hex string) uint64 { v, _ := hexutil.DecodeUint64(hex); return v }
func safeBig(v *big.Int) string {
	if v == nil {
		return ""
	}
	return v.String()
}

// osignalNotify is isolated for testability
func osignalNotify(ch chan<- os.Signal) { signal.Notify(ch, os.Interrupt) }

// ---- helpers for pretty printing (optional) ----
func formatFields(fields map[string]interface{}) string {
	if len(fields) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, fields[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
