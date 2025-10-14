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
	"github.com/yourusername/screner/internal/assets"
	"github.com/yourusername/screner/internal/dex/pricing"
	"github.com/yourusername/screner/internal/util"
	pb "github.com/yourusername/screner/pkg/protobuf"
)

// Config описывает минимальный набор настроек для подписки на Uniswap V2 пулы.
type Config struct {
	WSURL              string
	HTTPURL            string
	Exchange           string
	Network            string
	ChainID            uint64
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

// TokenRegistry предоставляет метаданные по стейблам и нативным токенам для текущей сети.
type TokenRegistry struct {
	byAddress      map[string]TokenMeta
	stableBySymbol map[string]TokenMeta
	nativeBySymbol map[string]TokenMeta
	wethAddr       string
	chainID        uint64
	network        string
}

// RegistryOptions задаёт параметры выбора сети для построения токенного справочника.
type RegistryOptions struct {
	Network   string
	NetworkID string
	ChainID   uint64
}

// NewTokenRegistry строит справочник токенов из провайдера активов с резервным набором адресов.
func NewTokenRegistry(provider *assets.Provider, opts RegistryOptions) *TokenRegistry {
	opts.Network = strings.TrimSpace(opts.Network)
	opts.NetworkID = strings.TrimSpace(opts.NetworkID)
	reg := &TokenRegistry{
		byAddress:      make(map[string]TokenMeta),
		stableBySymbol: make(map[string]TokenMeta),
		nativeBySymbol: make(map[string]TokenMeta),
		chainID:        opts.ChainID,
	}

	if provider != nil {
		loadNetwork := func(identifier string) bool {
			id := strings.TrimSpace(identifier)
			if id == "" {
				return false
			}
			stable := provider.TokensByNetwork(id, assets.TokenTypeStable)
			native := provider.TokensByNetwork(id, assets.TokenTypeNative)
			if len(stable) == 0 && len(native) == 0 {
				return false
			}
			if catalog, ok := provider.ResolveNetworkCatalog(id); ok {
				reg.network = catalog.Name
				if opts.ChainID == 0 {
					reg.chainID = catalog.Chain
				}
				provider.RegisterNetworkAlias(catalog.Name, id)
			}
			for _, token := range stable {
				reg.addStable(token.Symbol, token.Address, int(token.Decimals))
			}
			for _, token := range native {
				markWETH := token.Wrapped || strings.EqualFold(token.Symbol, "WETH")
				reg.addNative(token.Symbol, token.Address, int(token.Decimals), markWETH)
			}
			return true
		}

		loaded := false
		if !loaded && opts.NetworkID != "" {
			loaded = loadNetwork(opts.NetworkID)
		}
		if !loaded && opts.Network != "" {
			loaded = loadNetwork(opts.Network)
		}
		if !loaded && opts.ChainID != 0 {
			for _, token := range provider.TokensByChain(opts.ChainID, assets.TokenTypeStable) {
				reg.addStable(token.Symbol, token.Address, int(token.Decimals))
			}
			for _, token := range provider.TokensByChain(opts.ChainID, assets.TokenTypeNative) {
				markWETH := token.Wrapped || strings.EqualFold(token.Symbol, "WETH")
				reg.addNative(token.Symbol, token.Address, int(token.Decimals), markWETH)
			}
			if len(reg.stableBySymbol) > 0 {
				reg.chainID = opts.ChainID
				if catalog, ok := provider.ResolveNetworkByChain(opts.ChainID); ok {
					reg.network = catalog.Name
				}
				loaded = true
			}
		}
		if !loaded {
			for _, net := range provider.NetworkNames() {
				if loadNetwork(net) {
					loaded = true
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

// ChainID возвращает ChainID сети, для которой построен справочник (0, если не обнаружено).
func (r *TokenRegistry) ChainID() uint64 {
	if r == nil {
		return 0
	}
	return r.chainID
}

// NetworkName возвращает нормализованное имя сети, известное справочнику.
func (r *TokenRegistry) NetworkName() string {
	if r == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(r.network))
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
	cfg.Exchange = strings.ToLower(strings.TrimSpace(cfg.Exchange))
	cfg.Network = strings.ToLower(strings.TrimSpace(cfg.Network))
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
		info := c.makeTokenInfo(pool.Token0)
		c.pricer.RegisterToken(info)
		if pool.Token0.IsStable {
			c.pricer.RegisterStable(info)
		}
	}
	if (pool.Token1.Address != common.Address{}) {
		info := c.makeTokenInfo(pool.Token1)
		c.pricer.RegisterToken(info)
		if pool.Token1.IsStable {
			c.pricer.RegisterStable(info)
		}
	}
}

func (c *Connector) makeTokenInfo(meta TokenMeta) pricing.TokenInfo {
	info := pricing.TokenInfo{
		Address:  meta.Address,
		Symbol:   meta.Symbol,
		Decimals: meta.Decimals,
		DexAlias: c.exchangeName(),
		Network:  c.networkName(),
		ChainID:  c.cfg.ChainID,
	}
	if meta.IsStable {
		info.Bridge = strings.ToUpper(strings.TrimSpace(meta.Symbol))
	} else if meta.IsWETH {
		info.Bridge = "WETH"
	}
	return info
}

// Run запускает приём цен и публикует их в общий канал Screener Core.
func (c *Connector) Run(ctx context.Context, out chan<- *pb.MarketData) error {
	if len(c.cfg.Pools) == 0 {
		return errors.New("uniswap v2: no pools configured")
	}
	if c.dialer == nil {
		return errors.New("uniswap v2: dialer is nil")
	}

	util.Infof("uniswap_v2: starting run exchange=%s network=%s chain_id=%d pools=%d", c.exchangeName(), c.networkName(), c.cfg.ChainID, len(c.cfg.Pools))

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
	name := strings.TrimSpace(c.cfg.Exchange)
	if name != "" {
		return name
	}
	return "uniswap_v2"
}

func (c *Connector) networkName() string {
	network := strings.TrimSpace(c.cfg.Network)
	if network != "" {
		return network
	}
	return ""
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
	info0 := c.makeTokenInfo(meta.Token0)
	info1 := c.makeTokenInfo(meta.Token1)
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
		util.Infof("uniswap_v2: USD %s price=%.8f weight=%.4f route=%s network=%s chain_id=%d", marketSymbol, res.Price, res.Weight, route, c.networkName(), c.cfg.ChainID)
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
		Network:   c.networkName(),
		ChainID:   uint32(c.cfg.ChainID),
	}
	out <- md
	util.Debugf("uniswap_v2: emit symbol=%s price=%.8f exchange=%s network=%s chain_id=%d", symbol, value, md.Exchange, md.Network, c.cfg.ChainID)
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
	AMMVersion  string     `json:"amm_version"`
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

// PoolSourceOptions описывает дополнительные фильтры при загрузке пулов из общего источника.
type PoolSourceOptions struct {
	DexFilters     []string
	NetworkFilters []string
	WantedPairs    []string
	IncludeStable  bool
	AMMVersions    []string
}

var (
	hardWethStable = []struct {
		Addr   string
		Stable string
	}{
		{Addr: "0xB4e16d0168e52d35CaCD2c6185b44281Ec28C9Dc", Stable: "USDC"},
		{Addr: "0x0d4a11d5EEaaC28EC3F61d100daf4d40471f1852", Stable: "USDT"},
		{Addr: "0xA478c2975Ab1Ea89e8196811F51A7B7Ade33eB11", Stable: "DAI"},
	}
)

// LoadPoolsFromSource parses pools JSON (legacy GeckoTerminal or base_pools.json) and returns Uniswap V2 pools using the default registry.
func LoadPoolsFromSource(path string) ([]PoolConfig, error) {
	return LoadPoolsFromSourceWithOptions(path, nil, PoolSourceOptions{})
}

// LoadPoolsFromSourceWithRegistry allows passing an explicit token registry for pool normalization.
func LoadPoolsFromSourceWithRegistry(path string, registry *TokenRegistry) ([]PoolConfig, error) {
	return LoadPoolsFromSourceWithOptions(path, registry, PoolSourceOptions{})
}

// LoadPoolsFromSourceWithOptions загружает пулы с учётом фильтров по сети/DEX/парам.
func LoadPoolsFromSourceWithOptions(path string, registry *TokenRegistry, opts PoolSourceOptions) ([]PoolConfig, error) {
	if registry == nil {
		registry = NewTokenRegistry(nil, RegistryOptions{})
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file geckoPoolFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}

	allowedAMM := make(map[string]struct{})
	for _, want := range opts.AMMVersions {
		if norm := normalizeAMMVersion(want); norm != "" {
			allowedAMM[norm] = struct{}{}
		}
	}

	wanted := make(map[string]struct{})
	for _, pair := range opts.WantedPairs {
		key := strings.ToUpper(strings.TrimSpace(pair))
		if key == "" {
			continue
		}
		wanted[key] = struct{}{}
	}

	result := make([]PoolConfig, 0, len(file.Entries))
	seen := make(map[common.Address]bool)
	for _, entry := range file.Entries {
		if len(allowedAMM) > 0 {
			if _, ok := allowedAMM[normalizeAMMVersion(entry.AMMVersion)]; !ok {
				continue
			}
		}
		if len(opts.NetworkFilters) > 0 && !networkMatchesAny(entry.Network, opts.NetworkFilters) {
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

		t0 := normalizeToken(entry.Token0, registry)
		t1 := normalizeToken(entry.Token1, registry)

		stable0 := t0.IsStable
		stable1 := t1.IsStable
		weth0 := t0.IsWETH
		weth1 := t1.IsWETH

		if stable0 && stable1 && !opts.IncludeStable {
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
		if len(wanted) > 0 {
			key := strings.ToUpper(strings.TrimSpace(pool.CanonicalPair))
			if key == "" {
				key = strings.ToUpper(strings.ReplaceAll(pool.PairName, "/", ""))
			}
			if _, ok := wanted[key]; !ok {
				continue
			}
		}
		result = append(result, pool)
		seen[addr] = true
	}

	if len(result) == 0 && shouldAddHardFallback(opts, registry) {
		added := 0
		wethMeta, hasWETH := registry.WETHMeta()
		for _, hw := range hardWethStable {
			addr := common.HexToAddress(hw.Addr)
			if seen[addr] {
				continue
			}
			if len(opts.DexFilters) > 0 && !dexMatchesAny("uniswap", opts.DexFilters) {
				continue
			}
			stableMeta, ok := registry.StableBySymbol(hw.Stable)
			if !ok || !hasWETH {
				continue
			}
			pool := PoolConfig{
				Address:      addr,
				PairName:     fmt.Sprintf("WETH / %s (hard)", hw.Stable),
				Token0:       wethMeta,
				Token1:       stableMeta,
				BaseIsToken0: true,
			}
			FinalizePool(&pool)
			if len(wanted) > 0 {
				key := strings.ToUpper(strings.TrimSpace(pool.CanonicalPair))
				if _, ok := wanted[key]; !ok {
					continue
				}
			}
			result = append(result, pool)
			seen[addr] = true
			added++
		}
		if added > 0 {
			util.Infof("uniswap_v2: added %d hard reference pools", added)
		}
	}

	return result, nil
}

// LoadPoolsFromGecko is kept for backward compatibility.
func LoadPoolsFromGecko(path string) ([]PoolConfig, error) {
	return LoadPoolsFromSource(path)
}

// LoadPoolsFromGeckoWithRegistry is kept for backward compatibility.
func LoadPoolsFromGeckoWithRegistry(path string, registry *TokenRegistry) ([]PoolConfig, error) {
	return LoadPoolsFromSourceWithRegistry(path, registry)
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

func normalizeToken(token geckoToken, registry *TokenRegistry) TokenMeta {
	addrTrim := strings.TrimSpace(token.Address)
	addr := common.HexToAddress(addrTrim)
	meta := registry.Resolve(addr, token.Symbol, int(token.Decimals))
	if meta.Address == (common.Address{}) {
		meta.Address = addr
	}
	if meta.Symbol == "" {
		meta.Symbol = strings.ToUpper(shortAddress(addr.Hex()))
	}
	if meta.Decimals == 0 {
		meta.Decimals = 18
	}
	return meta
}

func shortAddress(addr string) string {
	addr = strings.TrimPrefix(addr, "0x")
	if len(addr) <= 6 {
		return strings.ToUpper(addr)
	}
	return strings.ToUpper(addr[:3] + addr[len(addr)-3:])
}

func shouldAddHardFallback(opts PoolSourceOptions, registry *TokenRegistry) bool {
	if len(opts.NetworkFilters) > 0 {
		for _, nf := range opts.NetworkFilters {
			if strings.Contains(strings.ToLower(nf), "eth") {
				return true
			}
		}
		return false
	}
	if registry != nil {
		net := strings.ToLower(strings.TrimSpace(registry.NetworkName()))
		if strings.Contains(net, "eth") {
			return true
		}
	}
	return true
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
