package uniswap

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/yourusername/screner/internal/dex/pricing"
	"github.com/yourusername/screner/internal/util"
	pb "github.com/yourusername/screner/pkg/protobuf"
)

// Config описывает минимальный набор настроек для подписки на Uniswap V2 пулы.
type Config struct {
	WSURL              string
	HTTPURL            string
	Exchange           string
	Pools              []PoolConfig
	SubscribeBatchSize int
	PingInterval       time.Duration
}

// PoolConfig хранит метаданные пула, необходимые для расчёта цены и канонического тикера.
type PoolConfig struct {
	Address       common.Address
	PairName      string
	Token0        TokenMeta
	Token1        TokenMeta
	BaseIsToken0  bool
	CanonicalPair string
	HasStable     bool
	HasWETH       bool
	StableSymbol  string
}

func FinalizePool(p *PoolConfig) {
	p.HasStable = p.Token0.IsStable || p.Token1.IsStable
	p.HasWETH = p.Token0.IsWETH || p.Token1.IsWETH
	switch {
	case p.Token0.IsStable:
		p.StableSymbol = p.Token0.Symbol
	case p.Token1.IsStable:
		p.StableSymbol = p.Token1.Symbol
	default:
		p.StableSymbol = ""
	}
	if p.CanonicalPair == "" {
		p.CanonicalPair = NormalizePair(*p)
	}
}

// TokenMeta описывает параметры токена внутри пула.
type TokenMeta struct {
	Address  common.Address
	Symbol   string
	Decimals int
	IsStable bool
	IsWETH   bool
}

// Dialer абстрагирует создание WebSocket соединения, что упростит тестирование и переиспользование логики.
type Dialer interface {
	Dial(ctx context.Context, endpoint string) (WSConnection, error)
}

// WSConnection формализует операции, которые нужны коннектору от WebSocket клиента.
type WSConnection interface {
	WriteJSON(v interface{}) error
	ReadMessage() (messageType int, data []byte, err error)
	WriteMessage(messageType int, data []byte) error
	Close() error
}

// Connector инкапсулирует логику подписки и обработки Uniswap V2.
type Connector struct {
	cfg    Config
	dialer Dialer
	pricer pricing.Pricer

	mu    sync.RWMutex
	pools map[common.Address]*poolState
}

// NewConnector создаёт коннектор с заданной конфигурацией и транспортом.
func NewConnector(cfg Config, dialer Dialer, pricer pricing.Pricer) *Connector {
	return &Connector{cfg: cfg, dialer: dialer, pricer: pricer}
}

type poolState struct {
	meta      PoolConfig
	lastPrice *big.Rat
	gotFirst  bool
}

type poolSnapshot struct {
	Price    *big.Rat
	Reserve0 *big.Int
	Reserve1 *big.Int
}

func (c *Connector) registerPoolTokens(pool PoolConfig) {
	if c.pricer == nil {
		return
	}
	if (pool.Token0.Address != common.Address{}) {
		info := pricing.TokenInfo{Address: pool.Token0.Address, Symbol: pool.Token0.Symbol, Decimals: pool.Token0.Decimals}
		c.pricer.RegisterToken(info)
		if pool.Token0.IsStable {
			c.pricer.RegisterStable(info)
		}
	}
	if (pool.Token1.Address != common.Address{}) {
		info := pricing.TokenInfo{Address: pool.Token1.Address, Symbol: pool.Token1.Symbol, Decimals: pool.Token1.Decimals}
		c.pricer.RegisterToken(info)
		if pool.Token1.IsStable {
			c.pricer.RegisterStable(info)
		}
	}
}

// Run запускает приём цен и публикует их в общий канал Screener Core.
func (c *Connector) Run(ctx context.Context, out chan<- *pb.MarketData) error {
	if len(c.cfg.Pools) == 0 {
		return errors.New("uniswap v2: no pools configured")
	}
	if c.dialer == nil {
		return errors.New("uniswap v2: dialer is nil")
	}

	pools := c.cfg.Pools
	if adjusted, err := AdjustPoolsOrdering(ctx, c.cfg.HTTPURL, pools); err != nil {
		util.Errorf("uniswap_v2: adjust pool order failed: %v", err)
	} else {
		pools = adjusted
		c.cfg.Pools = adjusted
	}

	c.pools = make(map[common.Address]*poolState, len(pools))
	for _, pool := range pools {
		ps := &poolState{meta: pool}
		c.pools[pool.Address] = ps
		c.registerPoolTokens(pool)
	}

	backoff := time.Second
	const backoffMax = 15 * time.Second

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		conn, err := c.dial(ctx)
		if err != nil {
			time.Sleep(backoff)
			backoff = nextBackoff(backoff, backoffMax)
			continue
		}

		if c.cfg.PingInterval > 0 {
			go c.keepAlive(ctx, conn, c.cfg.PingInterval)
		}

		if err := c.subscribeAll(conn); err != nil {
			_ = conn.Close()
			time.Sleep(backoff)
			backoff = nextBackoff(backoff, backoffMax)
			continue
		}

		backoff = time.Second
		readerCtx, cancel := context.WithCancel(ctx)
		errs := make(chan error, 1)
		messages := make(chan []byte, 1024)

		go c.readLoop(readerCtx, conn, messages, errs)

		run := true
		for run {
			select {
			case <-ctx.Done():
				cancel()
				_ = conn.Close()
				return ctx.Err()
			case err := <-errs:
				if err != nil {
					run = false
				}
			case raw, ok := <-messages:
				if !ok {
					run = false
					continue
				}
				if err := c.handleRaw(raw, out); err != nil {
					// Логирование оставляем внешним наблюдателям.
					_ = err
				}
			}
		}

		cancel()
		_ = conn.Close()
		time.Sleep(backoff)
		backoff = nextBackoff(backoff, backoffMax)
	}
}

// NormalizePair формирует каноническое имя пары (например, TOKENUSDT) на базе настроек пула.
func NormalizePair(pool PoolConfig) string {
	if pool.CanonicalPair != "" {
		return pool.CanonicalPair
	}
	base := pool.Token0.Symbol
	quote := pool.Token1.Symbol
	if !pool.BaseIsToken0 {
		base, quote = quote, base
	}
	return base + quote
}

// --- приватные утилиты ---

func (c *Connector) dial(ctx context.Context) (WSConnection, error) {
	endpoint := strings.TrimSpace(c.cfg.WSURL)
	if endpoint == "" {
		return nil, errors.New("uniswap v2: empty ws endpoint")
	}
	return c.dialer.Dial(ctx, endpoint)
}

func (c *Connector) subscribeAll(conn WSConnection) error {
	batchSize := c.cfg.SubscribeBatchSize
	if batchSize <= 0 {
		batchSize = 150
	}

	addresses := make([]string, 0, len(c.pools))
	for addr := range c.pools {
		addresses = append(addresses, addr.Hex())
	}

	id := 1
	for start := 0; start < len(addresses); start += batchSize {
		end := start + batchSize
		if end > len(addresses) {
			end = len(addresses)
		}
		req := rpcRequest{
			JSONRPC: "2.0",
			ID:      id,
			Method:  "eth_subscribe",
			Params: []interface{}{
				"logs",
				map[string]interface{}{
					"address": addresses[start:end],
				},
			},
		}
		if err := conn.WriteJSON(req); err != nil {
			return fmt.Errorf("subscribe batch %d: %w", id, err)
		}
		id++
	}
	return nil
}

func (c *Connector) readLoop(ctx context.Context, conn WSConnection, messages chan<- []byte, errs chan<- error) {
	defer close(messages)
	defer close(errs)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			errs <- err
			return
		}
		buf := make([]byte, len(data))
		copy(buf, data)
		messages <- buf
	}
}

func (c *Connector) handleRaw(raw []byte, out chan<- *pb.MarketData) error {
	if ack, ok := tryAck(raw); ok {
		if ack.Error != nil {
			return fmt.Errorf("uniswap v2 subscribe error %d %s", ack.Error.Code, ack.Error.Message)
		}
		return nil
	}
	note, ok := tryNote(raw)
	if !ok {
		return nil
	}

	poolAddr := common.HexToAddress(note.Params.Result.Address)
	c.mu.RLock()
	state, ok := c.pools[poolAddr]
	c.mu.RUnlock()
	if !ok {
		return nil
	}

	snapshot, err := computeSnapshot(state.meta, note.Params.Result.Data)
	if err != nil {
		return err
	}
	if snapshot == nil || snapshot.Price == nil {
		return nil
	}

	if drop := shouldDrop(state, snapshot.Price); drop {
		return nil
	}

	c.publish(state, snapshot, out)
	return nil
}

func (c *Connector) exchangeName() string {
	if strings.TrimSpace(c.cfg.Exchange) != "" {
		return c.cfg.Exchange
	}
	return "uniswap_v2"
}

const pingMessageType = 0x9

func (c *Connector) keepAlive(ctx context.Context, conn WSConnection, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = conn.WriteMessage(pingMessageType, nil)
		}
	}
}

func shouldDrop(state *poolState, price *big.Rat) bool {
	if state.lastPrice == nil {
		state.lastPrice = new(big.Rat).Set(price)
		return false
	}
	delta := new(big.Rat).Sub(price, state.lastPrice)
	if delta.Sign() == 0 {
		return true
	}
	threshold := new(big.Rat).Quo(state.lastPrice, big.NewRat(10000, 1))
	if delta.Abs(delta).Cmp(threshold) < 0 {
		return true
	}
	state.lastPrice = new(big.Rat).Set(price)
	return false
}

func (c *Connector) publish(state *poolState, snap *poolSnapshot, out chan<- *pb.MarketData) {
	if snap == nil || snap.Price == nil {
		return
	}
	meta := state.meta
	emitted := false
	pair := strings.ToUpper(strings.TrimSpace(meta.CanonicalPair))
	if pair == "" {
		pair = strings.ToUpper(baseSymbol(&meta) + quoteSymbol(&meta))
	}
	priceCopy := new(big.Rat).Set(snap.Price)
	if c.emitPrice(out, pair, priceCopy) {
		emitted = true
	}
	if c.pricer != nil {
		c.updatePricing(meta, snap, out)
	}
	if emitted && !state.gotFirst {
		state.gotFirst = true
	}
}

func (c *Connector) updatePricing(meta PoolConfig, snap *poolSnapshot, out chan<- *pb.MarketData) {
	if snap == nil || snap.Price == nil || c.pricer == nil {
		return
	}
	info0 := pricing.TokenInfo{Address: meta.Token0.Address, Symbol: meta.Token0.Symbol, Decimals: meta.Token0.Decimals}
	info1 := pricing.TokenInfo{Address: meta.Token1.Address, Symbol: meta.Token1.Symbol, Decimals: meta.Token1.Decimals}
	price := new(big.Rat).Set(snap.Price)
	var price1Per0, price0Per1 *big.Rat
	if meta.BaseIsToken0 {
		price1Per0 = price
		price0Per1 = invert(price)
	} else {
		price0Per1 = price
		price1Per0 = invert(price)
	}
	weight := reserveWeight(snap, meta)
	now := time.Now()
	if val, ok := ratToFloat(price1Per0); ok && val > 0 {
		c.pricer.UpdatePair(info0, info1, val, weight, now)
	} else if val, ok := ratToFloat(price0Per1); ok && val > 0 {
		c.pricer.UpdatePair(info1, info0, val, weight, now)
	} else {
		return
	}
	c.emitUSD(out, info0)
	c.emitUSD(out, info1)
}

func (c *Connector) emitUSD(out chan<- *pb.MarketData, info pricing.TokenInfo) {
	if c.pricer == nil {
		return
	}
	res, ok := c.pricer.ResolveUSD(info)
	if !ok || res.Price <= 0 || math.IsNaN(res.Price) || math.IsInf(res.Price, 0) {
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(info.Symbol))
	if symbol == "" {
		symbol = strings.ToUpper(strings.TrimPrefix(info.Address.Hex(), "0x"))
	}
	marketSymbol := symbol + "USD"
	if c.emitFloat(out, marketSymbol, res.Price) {
		route := strings.Join(res.Route, "->")
		if route == "" {
			route = "direct"
		}
		util.Infof("uniswap_v2: USD %s price=%.8f weight=%.4f route=%s", marketSymbol, res.Price, res.Weight, route)
	}
}

func reserveWeight(snap *poolSnapshot, meta PoolConfig) float64 {
	if snap == nil {
		return 1e-9
	}
	r0 := reserveToFloat(snap.Reserve0, meta.Token0.Decimals)
	r1 := reserveToFloat(snap.Reserve1, meta.Token1.Decimals)
	weight := math.Max(r0, r1)
	if weight <= 0 {
		return 1e-9
	}
	return weight
}

func reserveToFloat(reserve *big.Int, decimals int) float64 {
	if reserve == nil {
		return 0
	}
	abs := new(big.Int).Abs(new(big.Int).Set(reserve))
	if abs.Sign() == 0 {
		return 0
	}
	f := new(big.Float).SetPrec(256).SetInt(abs)
	if decimals > 0 {
		den := new(big.Float).SetPrec(256).SetInt(tenPow(uint(decimals)))
		f.Quo(f, den)
	}
	val, _ := f.Float64()
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return 0
	}
	return val
}

func (c *Connector) emitPrice(out chan<- *pb.MarketData, symbol string, value *big.Rat) bool {
	val, ok := ratToFloat(value)
	if !ok {
		return false
	}
	return c.emitFloat(out, symbol, val)
}

func (c *Connector) emitFloat(out chan<- *pb.MarketData, symbol string, value float64) bool {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return false
	}
	md := &pb.MarketData{
		Exchange:  c.exchangeName(),
		Symbol:    symbol,
		Price:     value,
		Timestamp: time.Now().UnixMilli(),
	}
	out <- md
	return true
}

func baseSymbol(meta *PoolConfig) string {
	if meta.BaseIsToken0 {
		return meta.Token0.Symbol
	}
	return meta.Token1.Symbol
}

func quoteSymbol(meta *PoolConfig) string {
	if meta.BaseIsToken0 {
		return meta.Token1.Symbol
	}
	return meta.Token0.Symbol
}

func tokenSymbol(meta *PoolConfig) string {
	if meta.HasStable && !meta.HasWETH {
		if meta.Token0.IsStable {
			return meta.Token1.Symbol
		}
		if meta.Token1.IsStable {
			return meta.Token0.Symbol
		}
	}
	if meta.HasWETH && !meta.HasStable {
		if meta.Token0.IsWETH {
			return meta.Token1.Symbol
		}
		if meta.Token1.IsWETH {
			return meta.Token0.Symbol
		}
	}
	return baseSymbol(meta)
}

func invert(r *big.Rat) *big.Rat {
	if r == nil || r.Sign() == 0 {
		return nil
	}
	return new(big.Rat).Inv(r)
}

func ratToFloat(r *big.Rat) (float64, bool) {
	if r == nil {
		return 0, false
	}
	f, _ := new(big.Float).SetPrec(256).SetRat(r).Float64()
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func formatRat(r *big.Rat, precision int) string {
	if r == nil {
		return ""
	}
	f := new(big.Float).SetPrec(256).SetRat(r)
	return f.Text('f', precision)
}

func computeSnapshot(pool PoolConfig, rawData string) (*poolSnapshot, error) {
	data := strings.TrimPrefix(rawData, "0x")
	if len(data) < 64*2 {
		return nil, errors.New("uniswap v2: invalid sync payload")
	}
	payload, err := hex.DecodeString(data)
	if err != nil {
		return nil, err
	}
	r0 := new(big.Int).SetBytes(payload[0:32])
	r1 := new(big.Int).SetBytes(payload[32:64])
	if r0.Sign() == 0 || r1.Sign() == 0 {
		return nil, nil
	}

	var price *big.Rat
	if pool.BaseIsToken0 {
		price = ratio(r1, pool.Token1.Decimals, r0, pool.Token0.Decimals)
	} else {
		price = ratio(r0, pool.Token0.Decimals, r1, pool.Token1.Decimals)
	}
	return &poolSnapshot{Price: price, Reserve0: r0, Reserve1: r1}, nil
}

func ratio(num *big.Int, numDec int, den *big.Int, denDec int) *big.Rat {
	if den.Sign() == 0 {
		return nil
	}
	numerator := new(big.Rat).SetFrac(num, tenPow(uint(numDec)))
	denominator := new(big.Rat).SetFrac(den, tenPow(uint(denDec)))
	return new(big.Rat).Quo(numerator, denominator)
}

func tenPow(dec uint) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil)
}

// --- RPC структуры ---

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type subAck struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  string `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type subNote struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		Subscription string   `json:"subscription"`
		Result       logEvent `json:"result"`
	} `json:"params"`
}

type logEvent struct {
	Address string   `json:"address"`
	Data    string   `json:"data"`
	Topics  []string `json:"topics"`
	Removed bool     `json:"removed"`
}

var syncTopic = "0x1c411e9a96e071241c2f21f7726b17ae89e3cab4c78be50e062b03a9fffbbad1"

func tryAck(raw []byte) (*subAck, bool) {
	var ack subAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		return nil, false
	}
	if ack.Result == "" && ack.Error == nil {
		return nil, false
	}
	return &ack, true
}

func tryNote(raw []byte) (*subNote, bool) {
	var note subNote
	if err := json.Unmarshal(raw, &note); err != nil {
		return nil, false
	}
	if !strings.EqualFold(note.Method, "eth_subscription") {
		return nil, false
	}
	if len(note.Params.Result.Topics) == 0 || !strings.EqualFold(note.Params.Result.Topics[0], syncTopic) {
		return nil, false
	}
	if note.Params.Result.Removed {
		return nil, false
	}
	return &note, true
}

func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		return max
	}
	if next < time.Second {
		return time.Second
	}
	return next
}

// --- Вспомогательные структуры и загрузчик пулов ---

type geckoPoolFile struct {
	Entries []geckoPool `json:"entries"`
}

type geckoPool struct {
	Dex         string     `json:"dex"`
	Network     string     `json:"network"`
	PairName    string     `json:"pair_name"`
	PoolID      string     `json:"pool_id"`
	PoolAddress string     `json:"pool_address"`
	Token0      geckoToken `json:"token0"`
	Token1      geckoToken `json:"token1"`
}

type geckoToken struct {
	Address  string      `json:"address"`
	Symbol   string      `json:"symbol"`
	Decimals intOrString `json:"decimals"`
}

var (
	wethAddressLower = strings.ToLower("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")
	stableSymbols    = map[string]bool{
		"USDC":  true,
		"USDT":  true,
		"DAI":   true,
		"TUSD":  true,
		"FDUSD": true,
		"USDD":  true,
		"USD1":  true,
	}
	canonicalTokens = map[string]struct {
		Symbol string
		Dec    uint8
	}{
		strings.ToLower("0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"): {Symbol: "USDC", Dec: 6},
		strings.ToLower("0xdac17f958d2ee523a2206206994597c13d831ec7"): {Symbol: "USDT", Dec: 6},
		strings.ToLower("0x6b175474e89094c44da98b954eedeac495271d0f"): {Symbol: "DAI", Dec: 18},
		strings.ToLower("0x0000000000085d4780B73119b644AE5ecd22b376"): {Symbol: "TUSD", Dec: 18},
		strings.ToLower("0x8d0d000ee44948fc98c9b98a4fa4921476f08b0d"): {Symbol: "USD1", Dec: 18},
		wethAddressLower: {Symbol: "WETH", Dec: 18},
	}
	hardWethStable = []struct {
		Addr   string
		Stable string
	}{
		{Addr: "0xB4e16d0168e52d35CaCD2c6185b44281Ec28C9Dc", Stable: "USDC"},
		{Addr: "0x0d4a11d5EEaaC28EC3F61d100daf4d40471f1852", Stable: "USDT"},
		{Addr: "0xA478c2975Ab1Ea89e8196811F51A7B7Ade33eB11", Stable: "DAI"},
	}
)

// LoadPoolsFromGecko парсит geckoterminal JSON и возвращает набор пулов Uniswap V2.
func LoadPoolsFromGecko(path string) ([]PoolConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file geckoPoolFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}

	result := make([]PoolConfig, 0, len(file.Entries))
	seen := make(map[common.Address]bool)
	for _, entry := range file.Entries {
		if !strings.EqualFold(entry.Dex, "uniswap_v2") {
			continue
		}
		if !strings.Contains(strings.ToLower(entry.Network), "eth") {
			continue
		}
		addrHex := entry.PoolID
		if addrHex == "" {
			addrHex = entry.PoolAddress
		}
		if !common.IsHexAddress(addrHex) {
			continue
		}
		addr := common.HexToAddress(addrHex)
		if seen[addr] {
			continue
		}

		t0 := normalizeToken(entry.Token0)
		t1 := normalizeToken(entry.Token1)

		stable0 := stableSymbols[t0.Symbol]
		stable1 := stableSymbols[t1.Symbol]
		weth0 := strings.ToLower(t0.Address.Hex()) == wethAddressLower
		weth1 := strings.ToLower(t1.Address.Hex()) == wethAddressLower

		if stable0 && stable1 {
			continue
		}
		if !stable0 && !stable1 && !weth0 && !weth1 {
			continue
		}

		baseIsToken0 := true
		switch {
		case (weth0 || weth1) && (stable0 || stable1):
			if weth0 && stable1 {
				baseIsToken0 = true
			} else if stable0 && weth1 {
				baseIsToken0 = false
			} else if weth0 {
				baseIsToken0 = true
			} else {
				baseIsToken0 = false
			}
		case stable0 || stable1:
			baseIsToken0 = !stable0
		case weth0 || weth1:
			baseIsToken0 = !weth0
		default:
			baseIsToken0 = true
		}

		pool := PoolConfig{
			Address:      addr,
			PairName:     entry.PairName,
			Token0:       t0,
			Token1:       t1,
			BaseIsToken0: baseIsToken0,
		}
		FinalizePool(&pool)
		result = append(result, pool)
		seen[addr] = true
	}

	added := 0
	for _, hw := range hardWethStable {
		addr := common.HexToAddress(hw.Addr)
		if seen[addr] {
			continue
		}
		stableSymbol := strings.ToUpper(hw.Stable)
		stableAddrHex := stableAddressForSymbol(stableSymbol)
		if stableAddrHex == "" {
			continue
		}
		stableDecimals := 18
		if stableSymbol == "USDC" || stableSymbol == "USDT" {
			stableDecimals = 6
		}
		stableMeta := NormalizeTokenMeta(stableSymbol, stableAddrHex, stableDecimals)
		wethMeta := NormalizeTokenMeta("WETH", "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2", 18)
		var pool PoolConfig
		switch stableSymbol {
		case "USDC":
			pool = PoolConfig{
				Address:      addr,
				PairName:     "USDC / WETH (hard)",
				Token0:       stableMeta,
				Token1:       wethMeta,
				BaseIsToken0: false,
			}
		case "USDT":
			pool = PoolConfig{
				Address:      addr,
				PairName:     "WETH / USDT (hard)",
				Token0:       wethMeta,
				Token1:       stableMeta,
				BaseIsToken0: true,
			}
		case "DAI":
			pool = PoolConfig{
				Address:      addr,
				PairName:     "DAI / WETH (hard)",
				Token0:       stableMeta,
				Token1:       wethMeta,
				BaseIsToken0: false,
			}
		default:
			continue
		}
		FinalizePool(&pool)
		result = append(result, pool)
		seen[addr] = true
		added++
	}
	if added > 0 {
		util.Infof("uniswap_v2: added %d hard reference pools", added)
	}

	return result, nil
}

// AdjustPoolsOrdering проверяет фактический порядок token0/token1 через RPC и при необходимости переставляет метаданные.
func AdjustPoolsOrdering(ctx context.Context, httpURL string, pools []PoolConfig) ([]PoolConfig, error) {
	trimmed := strings.TrimSpace(httpURL)
	if trimmed == "" {
		return pools, nil
	}
	client, err := rpc.DialContext(ctx, trimmed)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	decimalsCache := make(map[common.Address]int)

	for i := range pools {
		pool := &pools[i]
		if err := ensurePoolOrder(ctx, client, pool); err != nil {
			util.Debugf("uniswap_v2: ensurePoolOrder failed for %s (%s): %v", pool.PairName, pool.Address.Hex(), err)
		}
		updateTokenDecimals(ctx, client, pool, decimalsCache)
	}
	return pools, nil
}

func ensurePoolOrder(ctx context.Context, client *rpc.Client, pool *PoolConfig) error {
	t0, t1, err := fetchPairTokens(ctx, client, pool.Address)
	if err != nil {
		return err
	}
	if t0 == pool.Token0.Address && t1 == pool.Token1.Address {
		return nil
	}
	if t0 == pool.Token1.Address && t1 == pool.Token0.Address {
		swapPoolTokens(pool)
		FinalizePool(pool)
		return nil
	}
	return fmt.Errorf("token mismatch pool=%s meta0=%s meta1=%s actual0=%s actual1=%s", pool.PairName, pool.Token0.Address.Hex(), pool.Token1.Address.Hex(), t0.Hex(), t1.Hex())
}

func fetchPairTokens(ctx context.Context, client *rpc.Client, pair common.Address) (common.Address, common.Address, error) {
	t0, err := callAddress(ctx, client, pair, "0x0dfe1681")
	if err != nil {
		return common.Address{}, common.Address{}, err
	}
	t1, err := callAddress(ctx, client, pair, "0xd21220a7")
	if err != nil {
		return common.Address{}, common.Address{}, err
	}
	return t0, t1, nil
}

func callAddress(ctx context.Context, client *rpc.Client, pair common.Address, data string) (common.Address, error) {
	var result string
	call := map[string]string{"to": pair.Hex(), "data": data}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	if err := client.CallContext(ctx, &result, "eth_call", call, "latest"); err != nil {
		return common.Address{}, err
	}
	return parseAddressResult(result)
}

func parseAddressResult(res string) (common.Address, error) {
	res = strings.TrimSpace(res)
	if !strings.HasPrefix(res, "0x") {
		return common.Address{}, fmt.Errorf("unexpected eth_call result %s", res)
	}
	b, err := hex.DecodeString(strings.TrimPrefix(res, "0x"))
	if err != nil {
		return common.Address{}, err
	}
	if len(b) < 32 {
		return common.Address{}, fmt.Errorf("eth_call result too short: %d", len(b))
	}
	return common.BytesToAddress(b[12:]), nil
}

func swapPoolTokens(pool *PoolConfig) {
	pool.Token0, pool.Token1 = pool.Token1, pool.Token0
	pool.BaseIsToken0 = !pool.BaseIsToken0
}

func updateTokenDecimals(ctx context.Context, client *rpc.Client, pool *PoolConfig, cache map[common.Address]int) {
	updateMetaDecimals := func(meta *TokenMeta) {
		if meta.IsStable || meta.IsWETH {
			return
		}
		if meta.Decimals > 0 && meta.Decimals <= 18 && meta.Decimals >= 10 {
			return
		}
		if cached, ok := cache[meta.Address]; ok {
			meta.Decimals = cached
			return
		}
		if dec, err := fetchTokenDecimals(ctx, client, meta.Address); err == nil && dec > 0 {
			meta.Decimals = dec
			cache[meta.Address] = dec
			util.Infof("uniswap_v2: updated decimals addr=%s symbol=%s decimals=%d", meta.Address.Hex(), meta.Symbol, dec)
		} else if err != nil {
			util.Debugf("uniswap_v2: fetch decimals failed addr=%s: %v", meta.Address.Hex(), err)
		}
	}
	updateMetaDecimals(&pool.Token0)
	updateMetaDecimals(&pool.Token1)
	FinalizePool(pool)
}

func fetchTokenDecimals(ctx context.Context, client *rpc.Client, addr common.Address) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var result string
	call := map[string]string{"to": addr.Hex(), "data": "0x313ce567"}
	if err := client.CallContext(ctx, &result, "eth_call", call, "latest"); err != nil {
		return 0, err
	}
	res := strings.TrimPrefix(strings.TrimSpace(result), "0x")
	if res == "" {
		return 0, errors.New("empty decimals result")
	}
	data, err := hex.DecodeString(res)
	if err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, errors.New("no data for decimals")
	}
	dec := int(new(big.Int).SetBytes(data).Int64())
	if dec <= 0 {
		return 0, fmt.Errorf("invalid decimals %d", dec)
	}
	return dec, nil
}

func stableAddressForSymbol(sym string) string {
	switch strings.ToUpper(sym) {
	case "USDC":
		return "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"
	case "USDT":
		return "0xdac17f958d2ee523a2206206994597c13d831ec7"
	case "DAI":
		return "0x6b175474e89094c44da98b954eedeac495271d0f"
	case "TUSD":
		return "0x0000000000085d4780B73119b644AE5ecd22b376"
	default:
		return ""
	}
}

func normalizeToken(token geckoToken) TokenMeta {
	addrTrim := strings.TrimSpace(token.Address)
	addrLower := strings.ToLower(addrTrim)
	meta := TokenMeta{Address: common.HexToAddress(addrTrim)}
	if alias, ok := canonicalTokens[addrLower]; ok {
		meta.Symbol = alias.Symbol
		meta.Decimals = int(alias.Dec)
	} else {
		symbol := strings.ToUpper(strings.TrimSpace(token.Symbol))
		if symbol == "" {
			symbol = strings.ToUpper(shortAddress(token.Address))
		}
		meta.Symbol = symbol
		dec := int(token.Decimals)
		if dec <= 0 {
			if stableSymbols[meta.Symbol] {
				if meta.Symbol == "USDC" || meta.Symbol == "USDT" {
					dec = 6
				} else {
					dec = 18
				}
			} else {
				dec = 18
			}
		}
		if dec > 255 {
			dec = 18
		}
		meta.Decimals = dec
	}
	meta.IsStable = stableSymbols[meta.Symbol]
	meta.IsWETH = addrLower == wethAddressLower
	return meta
}

// NormalizeTokenMeta формирует TokenMeta из произвольных входных данных (для конфигурации).
func NormalizeTokenMeta(symbol, address string, decimals int) TokenMeta {
	meta := normalizeToken(geckoToken{
		Address:  address,
		Symbol:   symbol,
		Decimals: intOrString(decimals),
	})
	return meta
}

func shortAddress(addr string) string {
	addr = strings.TrimPrefix(addr, "0x")
	if len(addr) <= 6 {
		return strings.ToUpper(addr)
	}
	return strings.ToUpper(addr[:3] + addr[len(addr)-3:])
}

// intOrString поддерживает декод чисел GeckoTerminal
type intOrString int

func (v *intOrString) UnmarshalJSON(b []byte) error {
	bb := bytes.TrimSpace(b)
	if len(bb) == 0 {
		*v = 0
		return nil
	}
	if bb[0] == '"' {
		var s string
		if err := json.Unmarshal(bb, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*v = 0
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		*v = intOrString(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(bb, &n); err != nil {
		return err
	}
	*v = intOrString(n)
	return nil
}
