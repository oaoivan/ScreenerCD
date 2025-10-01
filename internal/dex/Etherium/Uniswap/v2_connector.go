package uniswap

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	pb "github.com/yourusername/screner/pkg/protobuf"
)

// Config описывает минимальный набор настроек для подписки на Uniswap V2 пулы.
type Config struct {
	WSURL              string
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
}

// TokenMeta описывает параметры токена внутри пула.
type TokenMeta struct {
	Address  common.Address
	Symbol   string
	Decimals uint8
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

	mu    sync.RWMutex
	pools map[common.Address]*poolState
}

// NewConnector создаёт коннектор с заданной конфигурацией и транспортом.
func NewConnector(cfg Config, dialer Dialer) *Connector {
	return &Connector{cfg: cfg, dialer: dialer}
}

type poolState struct {
	meta      PoolConfig
	lastPrice *big.Rat
}

// Run запускает приём цен и публикует их в общий канал Screener Core.
func (c *Connector) Run(ctx context.Context, out chan<- *pb.MarketData) error {
	if len(c.cfg.Pools) == 0 {
		return errors.New("uniswap v2: no pools configured")
	}
	if c.dialer == nil {
		return errors.New("uniswap v2: dialer is nil")
	}

	c.pools = make(map[common.Address]*poolState, len(c.cfg.Pools))
	for _, pool := range c.cfg.Pools {
		ps := &poolState{meta: pool}
		c.pools[pool.Address] = ps
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

	priceRat, err := computePrice(state.meta, note.Params.Result.Data)
	if err != nil {
		return err
	}
	if priceRat == nil {
		return nil
	}

	if drop := shouldDrop(state, priceRat); drop {
		return nil
	}

	price, _ := priceRat.Float64()
	md := &pb.MarketData{
		Exchange:  c.exchangeName(),
		Symbol:    NormalizePair(state.meta),
		Price:     price,
		Timestamp: time.Now().UnixMilli(),
	}
	out <- md
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

func computePrice(pool PoolConfig, rawData string) (*big.Rat, error) {
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

	if pool.BaseIsToken0 {
		return ratio(r1, int(pool.Token1.Decimals), r0, int(pool.Token0.Decimals)), nil
	}
	return ratio(r0, int(pool.Token0.Decimals), r1, int(pool.Token1.Decimals)), nil
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
	}
	canonicalTokens = map[string]struct {
		Symbol string
		Dec    uint8
	}{
		strings.ToLower("0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"): {Symbol: "USDC", Dec: 6},
		strings.ToLower("0xdac17f958d2ee523a2206206994597c13d831ec7"): {Symbol: "USDT", Dec: 6},
		strings.ToLower("0x6b175474e89094c44da98b954eedeac495271d0f"): {Symbol: "DAI", Dec: 18},
		strings.ToLower("0x0000000000085d4780B73119b644AE5ecd22b376"): {Symbol: "TUSD", Dec: 18},
		wethAddressLower: {Symbol: "WETH", Dec: 18},
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
		pool.CanonicalPair = NormalizePair(pool)
		result = append(result, pool)
		seen[addr] = true
	}

	return result, nil
}

func normalizeToken(token geckoToken) TokenMeta {
	addrTrim := strings.TrimSpace(token.Address)
	addrLower := strings.ToLower(addrTrim)
	meta := TokenMeta{Address: common.HexToAddress(addrTrim)}
	if alias, ok := canonicalTokens[addrLower]; ok {
		meta.Symbol = alias.Symbol
		meta.Decimals = alias.Dec
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
		meta.Decimals = uint8(dec)
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
