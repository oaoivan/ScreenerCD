package uniswap

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/yourusername/screner/internal/assets"
	"github.com/yourusername/screner/internal/dex/pricing"
	basepools "github.com/yourusername/screner/internal/pools/base"
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
		p.StableSymbol = symbolKey(p.Token0.Symbol)
	case p.Token1.IsStable:
		p.StableSymbol = symbolKey(p.Token1.Symbol)
	default:
		p.StableSymbol = ""
	}
	baseSymbol := symbolKey(p.Token0.Symbol)
	quoteSymbol := symbolKey(p.Token1.Symbol)
	if !p.BaseIsToken0 {
		baseSymbol, quoteSymbol = quoteSymbol, baseSymbol
	}
	if baseSymbol == "" {
		baseSymbol = p.Token0.Symbol
		if !p.BaseIsToken0 {
			baseSymbol = p.Token1.Symbol
		}
	}
	if quoteSymbol == "" {
		quoteSymbol = p.Token1.Symbol
		if !p.BaseIsToken0 {
			quoteSymbol = p.Token0.Symbol
		}
	}
	p.CanonicalPair = strings.ToUpper(baseSymbol + quoteSymbol)
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

// TokenRegistry предоставляет метаданные по стейблам и нативным токенам для текущей сети.
type TokenRegistry struct {
	byAddress      map[string]TokenMeta
	stableBySymbol map[string]TokenMeta
	nativeBySymbol map[string]TokenMeta
	wethAddr       string
}

// NewTokenRegistry строит справочник токенов из провайдера активов с резервным набором адресов.
func NewTokenRegistry(provider *assets.Provider, network string) *TokenRegistry {
	reg := &TokenRegistry{
		byAddress:      make(map[string]TokenMeta),
		stableBySymbol: make(map[string]TokenMeta),
		nativeBySymbol: make(map[string]TokenMeta),
	}

	addNetworkTokens := func(net string) {
		if net == "" {
			return
		}
		for _, token := range provider.TokensByNetwork(net, assets.TokenTypeStable) {
			reg.addStable(token.Symbol, token.Address, int(token.Decimals))
		}
		for _, token := range provider.TokensByNetwork(net, assets.TokenTypeNative) {
			// Wrapped нативы помечаем как WETH-эквиваленты.
			markWETH := token.Wrapped || strings.EqualFold(token.Symbol, "WETH")
			reg.addNative(token.Symbol, token.Address, int(token.Decimals), markWETH)
		}
	}

	if provider != nil {
		trimmed := strings.TrimSpace(network)
		if trimmed != "" {
			addNetworkTokens(trimmed)
		} else {
			for _, net := range provider.NetworkNames() {
				addNetworkTokens(net)
				if len(reg.stableBySymbol) > 0 && reg.wethAddr != "" {
					break
				}
			}
		}
	}

	if len(reg.stableBySymbol) == 0 {
		reg.addStable("USDC", common.HexToAddress("0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"), 6)
		reg.addStable("USDT", common.HexToAddress("0xdac17f958d2ee523a2206206994597c13d831ec7"), 6)
		reg.addStable("DAI", common.HexToAddress("0x6b175474e89094c44da98b954eedeac495271d0f"), 18)
		reg.addStable("TUSD", common.HexToAddress("0x0000000000085d4780B73119b644AE5ecd22b376"), 18)
		reg.addStable("USD1", common.HexToAddress("0x8d0d000ee44948fc98c9b98a4fa4921476f08b0d"), 18)
	}
	if reg.wethAddr == "" {
		reg.addNative("WETH", common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"), 18, true)
	}

	return reg
}

func (r *TokenRegistry) addStable(symbol string, addr common.Address, decimals int) {
	meta := TokenMeta{
		Address:  addr,
		Symbol:   symbolKey(symbol),
		Decimals: clampDecimals(decimals),
		IsStable: true,
	}
	key := meta.Symbol
	r.stableBySymbol[key] = meta
	r.byAddress[strings.ToLower(addr.Hex())] = meta
}

func (r *TokenRegistry) addNative(symbol string, addr common.Address, decimals int, isWETH bool) {
	meta := TokenMeta{
		Address:  addr,
		Symbol:   symbolKey(symbol),
		Decimals: clampDecimals(decimals),
		IsWETH:   isWETH || strings.EqualFold(symbol, "WETH"),
	}
	key := meta.Symbol
	r.nativeBySymbol[key] = meta
	r.byAddress[strings.ToLower(addr.Hex())] = meta
	if meta.IsWETH {
		r.wethAddr = strings.ToLower(addr.Hex())
	}
}

// Resolve возвращает метаданные токена по адресу/символу с учётом fallback-значений.
func (r *TokenRegistry) Resolve(addr common.Address, symbol string, decimals int) TokenMeta {
	addrLower := strings.ToLower(addr.Hex())
	if meta, ok := r.byAddress[addrLower]; ok {
		return meta
	}
	meta := TokenMeta{Address: addr}
	meta.Symbol = symbolKey(symbol)
	if meta.Symbol == "" {
		meta.Symbol = strings.ToUpper(shortAddress(addr.Hex()))
	}
	if stable, ok := r.stableBySymbol[meta.Symbol]; ok {
		meta.Decimals = stable.Decimals
		meta.IsStable = true
		return meta
	}
	if native, ok := r.nativeBySymbol[meta.Symbol]; ok {
		meta.Decimals = native.Decimals
		meta.IsWETH = native.IsWETH
		return meta
	}
	meta.Decimals = clampDecimals(decimals)
	if meta.Decimals == 0 {
		meta.Decimals = 18
	}
	if addrLower == r.wethAddr || meta.Symbol == "WETH" {
		meta.IsWETH = true
	}
	return meta
}

func (r *TokenRegistry) StableBySymbol(symbol string) (TokenMeta, bool) {
	meta, ok := r.stableBySymbol[symbolKey(symbol)]
	return meta, ok
}

// StableTokens возвращает копию списка стейблкоинов из реестра.
func (r *TokenRegistry) StableTokens() []TokenMeta {
	if r == nil {
		return nil
	}
	result := make([]TokenMeta, 0, len(r.stableBySymbol))
	for _, meta := range r.stableBySymbol {
		result = append(result, meta)
	}
	return result
}

// NativeTokens возвращает копию списка нативных токенов из реестра.
func (r *TokenRegistry) NativeTokens() []TokenMeta {
	if r == nil {
		return nil
	}
	result := make([]TokenMeta, 0, len(r.nativeBySymbol))
	for _, meta := range r.nativeBySymbol {
		result = append(result, meta)
	}
	return result
}

// WETHAddress возвращает адрес Wrapped ETH, если он известен в реестре.
func (r *TokenRegistry) WETHAddress() common.Address {
	if r == nil || r.wethAddr == "" {
		return common.Address{}
	}
	return common.HexToAddress(r.wethAddr)
}

func (r *TokenRegistry) WETHMeta() (TokenMeta, bool) {
	if r.wethAddr == "" {
		return TokenMeta{}, false
	}
	meta, ok := r.byAddress[r.wethAddr]
	return meta, ok
}

func (r *TokenRegistry) IsStableSymbol(symbol string) bool {
	_, ok := r.stableBySymbol[symbolKey(symbol)]
	return ok
}

func (r *TokenRegistry) IsWETHAddress(addr common.Address) bool {
	if r.wethAddr == "" {
		return false
	}
	return strings.ToLower(addr.Hex()) == r.wethAddr
}

func symbolKey(symbol string) string {
	return strings.ToUpper(strings.TrimSpace(symbol))
}

func clampDecimals(dec int) int {
	if dec <= 0 || dec > 255 {
		return 0
	}
	return dec
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
		price = ratio(r0, pool.Token0.Decimals, r1, pool.Token1.Decimals)
	} else {
		price = ratio(r1, pool.Token1.Decimals, r0, pool.Token0.Decimals)
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

// LoadPoolsFromBase загружает пулы из base_pools.json и возвращает конфигурации V2.

func LoadPoolsFromBase(path string, dexName string, network string) ([]PoolConfig, error) {
	filter := basepools.Filter{}
	if trimmed := strings.TrimSpace(dexName); trimmed != "" {
		filter.Dexes = []string{trimmed}
	}
	if trimmed := strings.TrimSpace(network); trimmed != "" {
		filter.Networks = []string{trimmed}
	}
	return LoadPoolsFromBaseWithRegistry(path, nil, filter)
}

// LoadPoolsFromBaseWithRegistry аналогичен LoadPoolsFromBase, но позволяет передать внешний реестр токенов.
func LoadPoolsFromBaseWithRegistry(path string, registry *TokenRegistry, filter basepools.Filter) ([]PoolConfig, error) {
	entries, err := basepools.LoadBasePools(path)
	if err != nil {
		return nil, err
	}

	if len(filter.Dexes) == 0 {
		filter.Dexes = []string{"uniswap"}
	}
	for i := range filter.Dexes {
		filter.Dexes[i] = basepools.NormalizeDex(filter.Dexes[i])
	}
	if len(filter.Versions) == 0 {
		filter.Versions = []basepools.Version{basepools.VersionV2}
	}
	if len(filter.Networks) > 0 {
		trimmedNetworks := make([]string, 0, len(filter.Networks))
		for _, net := range filter.Networks {
			if n := strings.TrimSpace(net); n != "" {
				trimmedNetworks = append(trimmedNetworks, n)
			}
		}
		filter.Networks = trimmedNetworks
	}
	filtered := basepools.FilterEntries(entries, filter)
	if len(filtered) == 0 {
		return nil, fmt.Errorf("uniswap_v2: no pools found in %s", path)
	}

	if registry == nil {
		registry = NewTokenRegistry(nil, "")
	}

	result := make([]PoolConfig, 0, len(filtered))
	seen := make(map[common.Address]struct{}, len(filtered))

	for _, entry := range filtered {
		addressHex := strings.TrimSpace(entry.PoolAddress)
		if addressHex == "" {
			addressHex = strings.TrimSpace(entry.PoolID)
		}
		poolAddress, err := parseRequiredAddress(addressHex)
		if err != nil {
			util.Debugf("uniswap_v2: skip pool symbol=%s err=%v", entry.Symbol, err)
			continue
		}
		if _, ok := seen[poolAddress]; ok {
			continue
		}
		seen[poolAddress] = struct{}{}

		token0 := resolveBaseToken(entry.Token0, registry)
		token1 := resolveBaseToken(entry.Token1, registry)
		// Uniswap V2 pairs sort token0/token1 lexicographically by address. Normalise order
		// so that our metadata matches the on-chain layout without performing RPC calls.
		if token0.Address.Big().Cmp(token1.Address.Big()) > 0 {
			token0, token1 = token1, token0
		}

		pool := PoolConfig{
			Address:      poolAddress,
			PairName:     normalizePair(entry.PairName, token0.Symbol, token1.Symbol),
			Token0:       token0,
			Token1:       token1,
			BaseIsToken0: determineBase(entry, token0, token1),
		}
		FinalizePool(&pool)
		result = append(result, pool)
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("uniswap_v2: no pools passed validation from %s", path)
	}
	return result, nil
}

func resolveBaseToken(token basepools.Token, registry *TokenRegistry) TokenMeta {
	addr := common.HexToAddress(strings.TrimSpace(token.Address))
	meta := registry.Resolve(addr, token.Symbol, token.Decimals)
	if meta.Address == (common.Address{}) {
		meta.Address = addr
	}
	if meta.Symbol == "" {
		meta.Symbol = symbolKey(token.Symbol)
	}
	if meta.Decimals == 0 {
		meta.Decimals = clampDecimals(token.Decimals)
		if meta.Decimals == 0 {
			meta.Decimals = 18
		}
	}
	return meta
}

func determineBase(entry basepools.Entry, token0, token1 TokenMeta) bool {
	baseHex := strings.ToLower(strings.TrimSpace(entry.BaseToken))
	quoteHex := strings.ToLower(strings.TrimSpace(entry.QuoteToken))
	token0Hex := strings.ToLower(token0.Address.Hex())
	token1Hex := strings.ToLower(token1.Address.Hex())

	switch {
	case baseHex != "" && baseHex == token0Hex:
		return true
	case baseHex != "" && baseHex == token1Hex:
		return false
	case quoteHex != "" && quoteHex == token0Hex:
		return false
	case quoteHex != "" && quoteHex == token1Hex:
		return true
	case token0.IsWETH && !token1.IsWETH:
		return false
	case token1.IsWETH && !token0.IsWETH:
		return true
	case token0.IsStable && !token1.IsStable:
		return false
	case token1.IsStable && !token0.IsStable:
		return true
	default:
		return true
	}
}

func swapPoolTokens(pool *PoolConfig) {
	pool.Token0, pool.Token1 = pool.Token1, pool.Token0
	pool.BaseIsToken0 = !pool.BaseIsToken0
}

func shortAddress(addr string) string {
	addr = strings.TrimPrefix(addr, "0x")
	if len(addr) <= 6 {
		return strings.ToUpper(addr)
	}
	return strings.ToUpper(addr[:3] + addr[len(addr)-3:])
}

func parseRequiredAddress(raw string) (common.Address, error) {
	addr := strings.TrimSpace(raw)
	if addr == "" {
		return common.Address{}, fmt.Errorf("empty address")
	}
	lower := strings.ToLower(addr)
	if strings.HasPrefix(lower, "0x") && len(addr) == 66 {
		addr = "0x" + addr[len(addr)-40:]
	}
	if !common.IsHexAddress(addr) {
		return common.Address{}, fmt.Errorf("invalid address %s", raw)
	}
	return common.HexToAddress(addr), nil
}

func normalizePair(current, symbol0, symbol1 string) string {
	name := strings.TrimSpace(current)
	if name == "" {
		name = fmt.Sprintf("%s/%s", strings.ToUpper(symbol0), strings.ToUpper(symbol1))
	}
	return strings.ToUpper(name)
}
