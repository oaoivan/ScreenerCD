package uniswap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/gorilla/websocket"
	"github.com/yourusername/screner/internal/dex/pricing"
	"github.com/yourusername/screner/internal/util"
	pb "github.com/yourusername/screner/pkg/protobuf"
)

const (
	poolManagerABIPath  = "ABI/Uniswap/V4/UniswapV4PoolManager.json"
	defaultPoolsPath    = "ticker_source/geckoterminal_pools.json"
	defaultExchangeName = "uniswap_v4"
)

var (
	twoPow192       = new(big.Int).Lsh(big.NewInt(1), 192)
	pow10Cache      sync.Map
	errOnceComplete = errors.New("uniswap_v4: once completed")
	errStablePool   = errors.New("uniswap_v4: stable-stable pool skipped")
)

// V4Config описывает параметры продакшн-коннектора Uniswap V4.
type V4Config struct {
	Exchange        string
	Network         string
	WSURL           string
	HTTPURL         string
	PoolManager     common.Address
	PoolsPath       string
	Pools           []V4PoolConfig
	SubscribeBatch  int
	PingInterval    time.Duration
	MaxMetaWorkers  int
	SwapOnly        bool
	LogAllEvents    bool
	StopOnAckError  bool
	Once            bool
	WantedPairs     []string
	WantedPairsOnly bool
	Registry        *TokenRegistry
}

// V4PoolConfig хранит метаданные пула для статической подписки.
type V4PoolConfig struct {
	PoolID        common.Hash
	PoolAddress   common.Address
	HookAddress   common.Address
	PairName      string
	Token0        TokenMeta
	Token1        TokenMeta
	BaseIsToken0  bool
	CanonicalPair string
}

// PoolMeta хранит runtime-метаданные пула и последнюю рассчитанную цену.
type PoolMeta struct {
	ID            common.Hash
	PairName      string
	Token0        TokenMeta
	Token1        TokenMeta
	BaseIsToken0  bool
	CanonicalPair string
	LastPrice     *big.Rat
	LastTick      int
	LastLiquidity *big.Int
	LastUpdate    time.Time
}

// GeckoEntry описывает запись GeckoTerminal с параметрами пула.
type GeckoEntry struct {
	Dex        string `json:"dex"`
	Network    string `json:"network"`
	lastUpdate time.Time
	PairName   string       `json:"pair_name"`
	PoolID     string       `json:"pool_id"`
	PoolAddr   string       `json:"pool_address"`
	Token0     GeckoToken   `json:"token0"`
	Token1     GeckoToken   `json:"token1"`
	PoolKey    GeckoPoolKey `json:"pool_key"`
}

type GeckoPoolKey struct {
	Hooks string `json:"hooks"`
}

// GeckoToken описывает структуру токена из GeckoTerminal.
type GeckoToken struct {
	Address  string         `json:"address"`
	Symbol   string         `json:"symbol"`
	Decimals geckoIntOrText `json:"decimals"`
}

// GeckoPayload агрегирует массив GeckoEntry.
type GeckoPayload struct {
	Entries []GeckoEntry `json:"entries"`
}

// geckoIntOrText поддерживает хранение decimals как строки или числа.
type geckoIntOrText int

func (v *geckoIntOrText) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*v = 0
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
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
		*v = geckoIntOrText(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*v = geckoIntOrText(n)
	return nil
}

// RPCRequest описывает JSON-RPC запрос для подписки на логи.
type RPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

// RPCError описывает стандартную ошибку JSON-RPC.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// SubAck описывает ack-ответ на подписку.
type SubAck struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      int       `json:"id"`
	Result  string    `json:"result"`
	Error   *RPCError `json:"error,omitempty"`
}

// SubNote описывает уведомление об очередном событии от eth_subscribe.
type SubNote struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		Subscription string   `json:"subscription"`
		Result       LogEvent `json:"result"`
	} `json:"params"`
}

// LogEvent описывает структуру Ethereum log.
type LogEvent struct {
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

type ackState int

const (
	ackNone ackState = iota
	ackSuccess
	ackRejected
)

type onceStatus struct {
	marked    bool
	remaining int
	completed bool
}

func (c *V4Connector) registerToken(meta TokenMeta) {
	if c.pricer == nil || meta.Address == (common.Address{}) {
		return
	}
	if _, loaded := c.registered.LoadOrStore(meta.Address, struct{}{}); loaded {
		return
	}
	info := pricing.TokenInfo{Address: meta.Address, Symbol: meta.Symbol, Decimals: meta.Decimals}
	c.pricer.RegisterToken(info)
	if meta.IsStable {
		c.pricer.RegisterStable(info)
	}
}

// V4Connector отвечает за подписку, парсинг и публикацию котировок V4.
type V4Connector struct {
	cfg    V4Config
	pricer pricing.Pricer

	poolsMu    sync.RWMutex
	pools      map[common.Hash]*v4PoolState
	registered sync.Map

	abiOnce    sync.Once
	abiErr     error
	managerABI abi.ABI
	eventBySig map[common.Hash]abi.Event

	successUpdates    uint64
	conversionErrors  uint64
	unknownPoolEvents uint64
}

// выборочное хранение состояния пула (lazy metadata, кэш цены и т.д.).
type v4PoolState struct {
	meta           V4PoolConfig
	registered     bool
	loaded         bool
	loadErr        error
	lastPrice      *big.Rat
	lastPrice1Per0 *big.Rat
	lastPrice0Per1 *big.Rat
	lastFloat1Per0 float64
	lastFloat0Per1 float64
	lastWeight     float64
	lastAmount0    *big.Int
	lastAmount1    *big.Int
	lastLiquidity  *big.Int
	lastTick       int64
	lastFee        *big.Int
	onceEmitted    bool
	lastUpdate     time.Time
}

// NewV4Connector создаёт коннектор с базовыми проверками параметров.
func NewV4Connector(cfg V4Config, pricer pricing.Pricer) (*V4Connector, error) {
	if pricer == nil {
		return nil, fmt.Errorf("uniswap_v4: pricer is required")
	}
	cfg.Exchange = strings.ToLower(strings.TrimSpace(cfg.Exchange))
	cfg.Network = strings.ToLower(strings.TrimSpace(cfg.Network))
	if cfg.Exchange == "" {
		cfg.Exchange = defaultExchangeName
	}
	if cfg.SubscribeBatch <= 0 {
		cfg.SubscribeBatch = 150
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 25 * time.Second
	}
	if cfg.MaxMetaWorkers <= 0 {
		cfg.MaxMetaWorkers = 4
	}
	if cfg.WSURL == "" || cfg.HTTPURL == "" {
		return nil, fmt.Errorf("uniswap_v4: both ws_url and http_url are required")
	}
	if cfg.PoolManager == (common.Address{}) {
		return nil, fmt.Errorf("uniswap_v4: pool_manager address is required")
	}

	util.Infof("uniswap_v4: init exchange=%s network=%s pools_inline=%d wanted_only=%v swap_only=%v", cfg.Exchange, cfg.Network, len(cfg.Pools), cfg.WantedPairsOnly, cfg.SwapOnly)

	return &V4Connector{
		cfg:    cfg,
		pricer: pricer,
		pools:  make(map[common.Hash]*v4PoolState),
	}, nil
}

func (c *V4Connector) exchangeName() string {
	name := strings.ToLower(strings.TrimSpace(c.cfg.Exchange))
	if name == "" {
		return defaultExchangeName
	}
	return name
}

func (c *V4Connector) tokenSymbol(meta TokenMeta) string {
	symbol := strings.ToUpper(strings.TrimSpace(meta.Symbol))
	if symbol != "" {
		return symbol
	}
	if meta.Address != (common.Address{}) {
		return strings.ToUpper(strings.TrimPrefix(meta.Address.Hex(), "0x"))
	}
	return ""
}

func (c *V4Connector) emitSpot(out chan<- *pb.MarketData, base TokenMeta, quote TokenMeta, price float64, ts time.Time) {
	if out == nil {
		return
	}
	if price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return
	}
	baseSymbol := c.tokenSymbol(base)
	quoteSymbol := c.tokenSymbol(quote)
	if baseSymbol == "" || quoteSymbol == "" {
		return
	}
	rawSymbol := baseSymbol + quoteSymbol
	symbol := util.NormalizeSpotSymbol(c.exchangeName(), rawSymbol)
	if symbol == "" {
		return
	}
	if ts.IsZero() {
		ts = time.Now()
	}
	md := &pb.MarketData{
		Exchange:  c.exchangeName(),
		Symbol:    symbol,
		Price:     price,
		Timestamp: ts.UnixMilli(),
	}
	out <- md
	if c.cfg.LogAllEvents {
		upperRaw := strings.ToUpper(rawSymbol)
		if symbol != upperRaw {
			util.Infof("uniswap_v4: spot symbol normalized raw=%s normalized=%s exchange=%s", upperRaw, symbol, md.Exchange)
		}
		util.Infof("uniswap_v4: spot %s price=%.10f exchange=%s", symbol, price, md.Exchange)
	}
}

func (c *V4Connector) ensureABI() error {
	c.abiOnce.Do(func() {
		util.Infof("uniswap_v4: loading pool manager ABI from %s", poolManagerABIPath)
		data, err := os.ReadFile(poolManagerABIPath)
		if err != nil {
			c.abiErr = fmt.Errorf("uniswap_v4: read ABI: %w", err)
			return
		}
		parsed, err := abi.JSON(bytes.NewReader(data))
		if err != nil {
			c.abiErr = fmt.Errorf("uniswap_v4: parse ABI: %w", err)
			return
		}
		c.managerABI = parsed
		events := make(map[common.Hash]abi.Event, len(parsed.Events))
		for _, evt := range parsed.Events {
			events[evt.ID] = evt
		}
		c.eventBySig = events
		util.Infof("uniswap_v4: ABI loaded events=%d", len(events))
	})
	return c.abiErr
}

// Run запускает главный цикл подписки и публикации котировок.
func (c *V4Connector) Run(ctx context.Context, out chan<- *pb.MarketData) error {
	util.Infof("uniswap_v4: starting connector run")
	if err := c.ensureABI(); err != nil {
		return err
	}
	if err := c.bootstrapPools(ctx); err != nil {
		return err
	}

	backoff := 2 * time.Second
	const backoffMax = 30 * time.Second

	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		conn, err := c.dial(ctx)
		if err != nil {
			util.Errorf("uniswap_v4: dial failed: %v", err)
			if !sleepWithContext(ctx, backoff) {
				return ctx.Err()
			}
			backoff = nextBackoffDuration(backoff, backoffMax)
			continue
		}

		util.Infof("uniswap_v4: websocket connected to %s", c.cfg.WSURL)

		if err := c.subscribe(conn); err != nil {
			util.Errorf("uniswap_v4: subscribe request failed: %v", err)
			_ = conn.Close()
			if !sleepWithContext(ctx, backoff) {
				return ctx.Err()
			}
			backoff = nextBackoffDuration(backoff, backoffMax)
			continue
		}

		streamErr := c.runStream(ctx, conn, out)
		_ = conn.Close()

		if streamErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if errors.Is(streamErr, errOnceComplete) {
				util.Infof("uniswap_v4: once mode completed, stopping run loop")
				return nil
			}
			util.Errorf("uniswap_v4: stream terminated: %v", streamErr)
			if !sleepWithContext(ctx, backoff) {
				return ctx.Err()
			}
			backoff = nextBackoffDuration(backoff, backoffMax)
			continue
		}

		backoff = 2 * time.Second
	}
}

// bootstrapPools формирует карту статических пулов перед запуском подписки.
func (c *V4Connector) bootstrapPools(ctx context.Context) error {
	util.Infof("uniswap_v4: bootstrap pools begin path=%s inline=%d", c.cfg.PoolsPath, len(c.cfg.Pools))

	wantedSet := make(map[string]struct{}, len(c.cfg.WantedPairs))
	for _, pair := range c.cfg.WantedPairs {
		key := pairKey(pair)
		if key != "" {
			wantedSet[key] = struct{}{}
		}
	}

	filterWanted := c.cfg.WantedPairsOnly
	if filterWanted && len(wantedSet) == 0 {
		return fmt.Errorf("uniswap_v4: wanted_pairs_only enabled but list empty")
	}

	prepared := make([]V4PoolConfig, 0, len(c.cfg.Pools))
	for idx, pool := range c.cfg.Pools {
		if err := ctxErr(ctx); err != nil {
			return err
		}
		normalized, err := c.normalizeInlinePool(pool, wantedSet, filterWanted)
		if err != nil {
			return fmt.Errorf("uniswap_v4: inline pool %d: %w", idx, err)
		}
		if normalized == nil {
			continue
		}
		prepared = append(prepared, *normalized)
	}

	path := strings.TrimSpace(c.cfg.PoolsPath)
	var fileCount int
	if len(prepared) == 0 {
		poolsFromFile, err := c.loadPoolsFromFile(ctx, path, wantedSet, filterWanted)
		if err != nil {
			return err
		}
		fileCount = len(poolsFromFile)
		prepared = append(prepared, poolsFromFile...)
	}

	if len(prepared) == 0 {
		return fmt.Errorf("uniswap_v4: no pools resolved (inline=%d path=%s)", len(c.cfg.Pools), path)
	}

	var (
		missingPoolAddr int
		missingHookAddr int
	)

	final := make(map[common.Hash]*v4PoolState, len(prepared))
	seen := make(map[common.Hash]struct{}, len(prepared))
	for _, pool := range prepared {
		if err := ctxErr(ctx); err != nil {
			return err
		}
		if _, ok := seen[pool.PoolID]; ok {
			util.Infof("uniswap_v4: skip duplicate pool id=%s pair=%s", pool.PoolID.Hex(), pool.PairName)
			continue
		}
		seen[pool.PoolID] = struct{}{}
		metaCopy := pool
		final[pool.PoolID] = &v4PoolState{meta: metaCopy}
		c.registerToken(pool.Token0)
		c.registerToken(pool.Token1)
		poolAddr := "-"
		if pool.PoolAddress != (common.Address{}) {
			poolAddr = strings.ToLower(pool.PoolAddress.Hex())
		} else {
			missingPoolAddr++
		}
		hookAddr := "-"
		if pool.HookAddress != (common.Address{}) {
			hookAddr = strings.ToLower(pool.HookAddress.Hex())
		} else {
			missingHookAddr++
		}
		util.Infof("uniswap_v4: pool ready pair=%s id=%s pool_addr=%s hook=%s base=%s quote=%s canon=%s dec0=%d dec1=%d", pool.PairName, pool.PoolID.Hex(), poolAddr, hookAddr, poolBaseSymbol(&pool), poolQuoteSymbol(&pool), pool.CanonicalPair, pool.Token0.Decimals, pool.Token1.Decimals)
	}

	c.poolsMu.Lock()
	c.pools = final
	c.poolsMu.Unlock()

	if missingPoolAddr > 0 || missingHookAddr > 0 {
		util.Infof("uniswap_v4: pools missing addresses pool_addr=%d hook_addr=%d", missingPoolAddr, missingHookAddr)
	}

	util.Infof("uniswap_v4: bootstrap pools done total=%d inline=%d file=%d wanted_only=%v", len(final), len(c.cfg.Pools), fileCount, filterWanted)
	return nil
}

// normalizeInlinePool валидирует inline-конфиги и приводит метаданные к единому виду.
func (c *V4Connector) normalizeInlinePool(pool V4PoolConfig, wanted map[string]struct{}, filter bool) (*V4PoolConfig, error) {
	if filter {
		if _, ok := wanted[pairKey(pool.PairName)]; !ok {
			util.Debugf("uniswap_v4: skip inline pool pair=%s by filter", pool.PairName)
			return nil, nil
		}
	}
	if pool.PoolID == (common.Hash{}) {
		return nil, fmt.Errorf("missing pool_id for pair=%s", pool.PairName)
	}

	token0, err := c.enrichTokenMeta(pool.Token0)
	if err != nil {
		return nil, fmt.Errorf("token0: %w", err)
	}
	token1, err := c.enrichTokenMeta(pool.Token1)
	if err != nil {
		return nil, fmt.Errorf("token1: %w", err)
	}

	normalized := pool
	normalized.Token0 = token0
	normalized.Token1 = token1
	normalized.PairName = normalizePairName(normalized.PairName, token0.Symbol, token1.Symbol)
	if normalized.CanonicalPair == "" {
		normalized.CanonicalPair = canonicalPair(normalized)
	}
	return &normalized, nil
}

// loadPoolsFromFile читает JSON GeckoTerminal и собирает список пулов.
func (c *V4Connector) loadPoolsFromFile(ctx context.Context, path string, wanted map[string]struct{}, filter bool) ([]V4PoolConfig, error) {
	if path == "" {
		path = defaultPoolsPath
	}
	util.Infof("uniswap_v4: loading pools from file=%s", path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("uniswap_v4: read pools file: %w", err)
	}
	var payload GeckoPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("uniswap_v4: decode pools file: %w", err)
	}
	util.Infof("uniswap_v4: pools file entries=%d", len(payload.Entries))

	result := make([]V4PoolConfig, 0, len(payload.Entries))
	networkFilter := strings.TrimSpace(c.cfg.Network)
	skippedNetwork := 0
	for _, entry := range payload.Entries {
		if err := ctxErr(ctx); err != nil {
			return nil, err
		}
		if !strings.EqualFold(entry.Dex, "uniswap_v4") {
			continue
		}
		if networkFilter != "" && !strings.EqualFold(entry.Network, networkFilter) {
			skippedNetwork++
			continue
		}
		if filter {
			if _, ok := wanted[pairKey(entry.PairName)]; !ok {
				continue
			}
		}
		pool, err := c.poolFromGecko(entry)
		if err != nil {
			if errors.Is(err, errStablePool) {
				util.Infof("uniswap_v4: skip stable pool pair=%s pool_id=%s token0=%s token1=%s", normalizePairName(entry.PairName, entry.Token0.Symbol, entry.Token1.Symbol), strings.TrimSpace(entry.PoolID), strings.ToUpper(strings.TrimSpace(entry.Token0.Symbol)), strings.ToUpper(strings.TrimSpace(entry.Token1.Symbol)))
				continue
			}
			util.Errorf("uniswap_v4: skip pool %s: %v", entry.PairName, err)
			continue
		}
		result = append(result, pool)
	}

	util.Infof("uniswap_v4: pools loaded from file=%d skipped_network=%d filter=%s", len(result), skippedNetwork, networkFilter)
	return result, nil
}

// poolFromGecko конвертирует запись GeckoTerminal в конфиг пула V4.
func (c *V4Connector) poolFromGecko(entry GeckoEntry) (V4PoolConfig, error) {
	poolID := strings.TrimSpace(entry.PoolID)
	if !strings.HasPrefix(poolID, "0x") || len(poolID) != 66 {
		return V4PoolConfig{}, fmt.Errorf("invalid pool_id=%s", entry.PoolID)
	}
	pid := common.HexToHash(poolID)

	token0, err := c.buildTokenMeta(entry.Token0.Address, entry.Token0.Symbol, int(entry.Token0.Decimals))
	if err != nil {
		return V4PoolConfig{}, fmt.Errorf("token0 %s: %w", entry.Token0.Symbol, err)
	}
	token1, err := c.buildTokenMeta(entry.Token1.Address, entry.Token1.Symbol, int(entry.Token1.Decimals))
	if err != nil {
		return V4PoolConfig{}, fmt.Errorf("token1 %s: %w", entry.Token1.Symbol, err)
	}

	baseIsToken0 := chooseBaseToken(token0, token1)
	pairName := normalizePairName(entry.PairName, token0.Symbol, token1.Symbol)

	if token0.IsStable && token1.IsStable {
		return V4PoolConfig{}, fmt.Errorf("%w pair=%s", errStablePool, pairName)
	}

	pool := V4PoolConfig{
		PoolID:        pid,
		PoolAddress:   parseOptionalAddress(entry.PoolAddr),
		HookAddress:   parseOptionalAddress(entry.PoolKey.Hooks),
		PairName:      pairName,
		Token0:        token0,
		Token1:        token1,
		BaseIsToken0:  baseIsToken0,
		CanonicalPair: canonicalPair(V4PoolConfig{Token0: token0, Token1: token1, BaseIsToken0: baseIsToken0}),
	}
	return pool, nil
}

func parseOptionalAddress(raw string) common.Address {
	addr := strings.TrimSpace(raw)
	if addr == "" {
		return common.Address{}
	}
	if strings.HasPrefix(strings.ToLower(addr), "0x") && len(addr) == 66 {
		// 32-byte value (likely pool_id) — not an address, silently ignore
		return common.Address{}
	}
	if !common.IsHexAddress(addr) {
		util.Infof("uniswap_v4: skip invalid address=%s", raw)
		return common.Address{}
	}
	return common.HexToAddress(addr)
}

// buildTokenMeta нормализует токен из JSON и обогащает его через реестр.
func (c *V4Connector) buildTokenMeta(addr, symbol string, decimals int) (TokenMeta, error) {
	addrTrim := strings.TrimSpace(addr)
	if !common.IsHexAddress(addrTrim) {
		return TokenMeta{}, fmt.Errorf("invalid address=%s", addr)
	}
	address := common.HexToAddress(addrTrim)
	meta := TokenMeta{Address: address, Symbol: strings.ToUpper(strings.TrimSpace(symbol)), Decimals: clampDecimals(decimals)}
	if c.cfg.Registry != nil {
		resolved := c.cfg.Registry.Resolve(address, symbol, decimals)
		if resolved.Address != (common.Address{}) {
			meta = resolved
		} else {
			meta.IsStable = resolved.IsStable
			meta.IsWETH = resolved.IsWETH
			if resolved.Symbol != "" {
				meta.Symbol = resolved.Symbol
			}
			if resolved.Decimals != 0 {
				meta.Decimals = clampDecimals(resolved.Decimals)
			}
		}
	}
	if meta.Symbol == "" {
		meta.Symbol = strings.ToUpper(shortAddress(address.Hex()))
	}
	meta.Decimals = clampDecimals(meta.Decimals)
	if meta.Decimals == 0 {
		meta.Decimals = 18
	}
	if meta.Decimals <= 0 {
		return TokenMeta{}, fmt.Errorf("invalid decimals for %s", meta.Symbol)
	}
	return meta, nil
}

// enrichTokenMeta приводит inline-описания токенов к формату с валидным символом и decimals.
func (c *V4Connector) enrichTokenMeta(meta TokenMeta) (TokenMeta, error) {
	if meta.Address == (common.Address{}) {
		return TokenMeta{}, fmt.Errorf("empty address")
	}
	symbol := strings.ToUpper(strings.TrimSpace(meta.Symbol))
	if c.cfg.Registry != nil {
		resolved := c.cfg.Registry.Resolve(meta.Address, symbol, meta.Decimals)
		if resolved.Address != (common.Address{}) {
			meta = resolved
		} else {
			if resolved.Symbol != "" {
				symbol = resolved.Symbol
			}
			if resolved.Decimals != 0 {
				meta.Decimals = clampDecimals(resolved.Decimals)
			}
			meta.IsStable = meta.IsStable || resolved.IsStable
			meta.IsWETH = meta.IsWETH || resolved.IsWETH
		}
	}
	if symbol == "" {
		symbol = strings.ToUpper(shortAddress(meta.Address.Hex()))
	}
	meta.Symbol = symbol
	meta.Decimals = clampDecimals(meta.Decimals)
	if meta.Decimals == 0 {
		meta.Decimals = 18
	}
	if meta.Decimals <= 0 {
		return TokenMeta{}, fmt.Errorf("invalid decimals for %s", meta.Symbol)
	}
	return meta, nil
}

// chooseBaseToken определяет базовую сторону пары, отдавая приоритет нестейбл токену.
func chooseBaseToken(token0, token1 TokenMeta) bool {
	switch {
	case token0.IsStable && !token1.IsStable:
		return false
	case token1.IsStable && !token0.IsStable:
		return true
	default:
		return true
	}
}

// canonicalPair строит канонический идентификатор пары.
func canonicalPair(pool V4PoolConfig) string {
	base := pool.Token0.Symbol
	quote := pool.Token1.Symbol
	if !pool.BaseIsToken0 {
		base, quote = quote, base
	}
	return base + quote
}

// normalizePairName приводит название пары к формату TOKEN/QUOTE.
func normalizePairName(current, symbol0, symbol1 string) string {
	name := strings.TrimSpace(current)
	if name == "" {
		name = fmt.Sprintf("%s/%s", strings.ToUpper(symbol0), strings.ToUpper(symbol1))
	}
	return strings.ToUpper(name)
}

// poolBaseSymbol возвращает символ базового токена.
func poolBaseSymbol(pool *V4PoolConfig) string {
	if pool.BaseIsToken0 {
		return pool.Token0.Symbol
	}
	return pool.Token1.Symbol
}

// poolQuoteSymbol возвращает символ котируемого токена.
func poolQuoteSymbol(pool *V4PoolConfig) string {
	if pool.BaseIsToken0 {
		return pool.Token1.Symbol
	}
	return pool.Token0.Symbol
}

// pairKey нормализует имя пары для фильтров.
func pairKey(name string) string {
	return strings.ToUpper(strings.TrimSpace(name))
}

// ctxErr проверяет отмену контекста, чтобы прерывать длительные операции.
func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (c *V4Connector) dial(ctx context.Context) (*websocket.Conn, error) {
	endpoint := strings.TrimSpace(c.cfg.WSURL)
	if endpoint == "" {
		return nil, fmt.Errorf("uniswap_v4: empty ws url")
	}
	dialer := websocket.Dialer{
		HandshakeTimeout:  15 * time.Second,
		EnableCompression: true,
	}
	conn, resp, err := dialer.DialContext(ctx, endpoint, nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(2 << 20) // 2 MiB safety limit
	util.Infof("uniswap_v4: handshake complete subprotocol=%s", conn.Subprotocol())
	return conn, nil
}

func (c *V4Connector) subscriptionAddresses() []string {
	set := make(map[string]struct{})
	if c.cfg.PoolManager != (common.Address{}) {
		set[strings.ToLower(c.cfg.PoolManager.Hex())] = struct{}{}
	}
	c.poolsMu.RLock()
	for _, state := range c.pools {
		if addr := state.meta.PoolAddress; addr != (common.Address{}) {
			set[strings.ToLower(addr.Hex())] = struct{}{}
		}
		if hook := state.meta.HookAddress; hook != (common.Address{}) {
			set[strings.ToLower(hook.Hex())] = struct{}{}
		}
	}
	c.poolsMu.RUnlock()
	if len(set) == 0 {
		return nil
	}
	result := make([]string, 0, len(set))
	for addr := range set {
		result = append(result, addr)
	}
	sort.Strings(result)
	return result
}

func (c *V4Connector) subscriptionPoolIDs() []string {
	c.poolsMu.RLock()
	defer c.poolsMu.RUnlock()
	if len(c.pools) == 0 {
		return nil
	}
	result := make([]string, 0, len(c.pools))
	for id := range c.pools {
		result = append(result, strings.ToLower(id.Hex()))
	}
	sort.Strings(result)
	return result
}

func (c *V4Connector) subscribe(conn *websocket.Conn) error {
	addresses := c.subscriptionAddresses()
	if len(addresses) == 0 {
		addresses = []string{strings.ToLower(c.cfg.PoolManager.Hex())}
	}
	poolIDs := c.subscriptionPoolIDs()
	filter := map[string]interface{}{
		"address": addresses,
	}
	if evt, ok := c.managerABI.Events["Swap"]; ok {
		topics := []interface{}{evt.ID.Hex()}
		if len(poolIDs) > 0 {
			topics = append(topics, poolIDs)
		}
		filter["topics"] = topics
	}
	util.Infof("uniswap_v4: subscribe filters addresses=%d pool_ids=%d", len(addresses), len(poolIDs))
	req := RPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_subscribe",
		Params: []interface{}{
			"logs",
			filter,
		},
	}
	if err := conn.WriteJSON(req); err != nil {
		return err
	}
	if payload, err := json.Marshal(req); err == nil {
		util.Infof("uniswap_v4: subscribe sent %s", string(payload))
	}
	return nil
}

func (c *V4Connector) runStream(ctx context.Context, conn *websocket.Conn, out chan<- *pb.MarketData) error {
	messages := make(chan []byte, 512)
	errs := make(chan error, 1)

	go c.readLoop(ctx, conn, messages, errs)
	if c.cfg.PingInterval > 0 {
		go c.keepAlive(ctx, conn, c.cfg.PingInterval)
	}

	const (
		ackDeadline = 15 * time.Second
		ackRetryMax = 5
	)

	var (
		ackTimer    *time.Timer
		ackCh       <-chan time.Time
		waitingAck  bool
		ackFailures int
	)

	startAckWait := func() {
		if ackTimer != nil {
			if !ackTimer.Stop() {
				select {
				case <-ackTimer.C:
				default:
				}
			}
		}
		ackTimer = time.NewTimer(ackDeadline)
		ackCh = ackTimer.C
		waitingAck = true
	}

	stopAckWait := func() {
		if ackTimer != nil {
			if !ackTimer.Stop() {
				select {
				case <-ackTimer.C:
				default:
				}
			}
			ackTimer = nil
		}
		ackCh = nil
		waitingAck = false
	}

	ackBackoff := func(attempt int) time.Duration {
		if attempt < 1 {
			attempt = 1
		}
		delay := time.Duration(attempt) * 2 * time.Second
		if delay > 10*time.Second {
			delay = 10 * time.Second
		}
		return delay
	}

	startAckWait()
	defer func() {
		if ackTimer != nil {
			_ = ackTimer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errs:
			if err != nil {
				return err
			}
			return nil
		case <-ackCh:
			if !waitingAck {
				continue
			}
			ackFailures++
			util.Errorf("uniswap_v4: subscription ack timeout attempt=%d", ackFailures)
			if c.cfg.StopOnAckError || ackFailures >= ackRetryMax {
				return fmt.Errorf("uniswap_v4: subscription ack timeout after %d attempts", ackFailures)
			}
			delay := ackBackoff(ackFailures)
			util.Infof("uniswap_v4: retry subscribe after timeout delay=%s attempt=%d", delay, ackFailures)
			if !sleepWithContext(ctx, delay) {
				return ctx.Err()
			}
			if err := c.subscribe(conn); err != nil {
				util.Errorf("uniswap_v4: resubscribe failed after timeout: %v", err)
				return err
			}
			startAckWait()
		case raw, ok := <-messages:
			if !ok {
				return fmt.Errorf("uniswap_v4: reader closed")
			}
			state, ackPayload, err := c.handleFrame(ctx, raw, out)
			if err != nil {
				return err
			}
			switch state {
			case ackSuccess:
				if ackFailures > 0 {
					util.Infof("uniswap_v4: subscription acknowledged after %d retries", ackFailures)
				}
				ackFailures = 0
				stopAckWait()
			case ackRejected:
				ackFailures++
				stopAckWait()
				var desc string
				if ackPayload != nil && ackPayload.Error != nil {
					desc = fmt.Sprintf("code=%d message=%s", ackPayload.Error.Code, ackPayload.Error.Message)
				} else {
					desc = "unknown reason"
				}
				util.Errorf("uniswap_v4: subscription rejected attempt=%d %s", ackFailures, desc)
				if c.cfg.StopOnAckError || ackFailures >= ackRetryMax {
					return fmt.Errorf("uniswap_v4: subscription rejected after %d attempts (%s)", ackFailures, desc)
				}
				delay := ackBackoff(ackFailures)
				util.Infof("uniswap_v4: retry subscribe after rejection delay=%s attempt=%d", delay, ackFailures)
				if !sleepWithContext(ctx, delay) {
					return ctx.Err()
				}
				if err := c.subscribe(conn); err != nil {
					util.Errorf("uniswap_v4: resubscribe failed after rejection: %v", err)
					return err
				}
				startAckWait()
			}
		}
	}
}

func (c *V4Connector) readLoop(ctx context.Context, conn *websocket.Conn, messages chan<- []byte, errs chan<- error) {
	defer close(messages)
	defer close(errs)

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			errs <- err
			return
		}

		switch mt {
		case websocket.PongMessage:
			util.Debugf("uniswap_v4: pong received")
			continue
		case websocket.PingMessage:
			_ = conn.WriteMessage(websocket.PongMessage, nil)
			continue
		case websocket.CloseMessage:
			errs <- fmt.Errorf("uniswap_v4: close frame received")
			return
		}

		payload := make([]byte, len(data))
		copy(payload, data)

		select {
		case messages <- payload:
		case <-ctx.Done():
			return
		}
	}
}

func (c *V4Connector) keepAlive(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				util.Errorf("uniswap_v4: ping failed: %v", err)
				return
			}
			util.Debugf("uniswap_v4: ping sent")
		}
	}
}

func (c *V4Connector) handleFrame(ctx context.Context, raw []byte, out chan<- *pb.MarketData) (ackState, *SubAck, error) {
	if ack, ok := parseAck(raw); ok {
		if ack.Error != nil {
			util.Errorf("uniswap_v4: subscribe error %d %s", ack.Error.Code, ack.Error.Message)
			return ackRejected, ack, nil
		}
		util.Infof("uniswap_v4: subscribe acknowledged id=%s", ack.Result)
		return ackSuccess, ack, nil
	}

	if note, ok := parseNote(raw); ok {
		return ackNone, nil, c.handleLog(ctx, note.Params.Result, out)
	}

	return ackNone, nil, nil
}

func (c *V4Connector) storeSwapMetrics(poolID common.Hash, canonicalPrice, price1Per0, price0Per1 *big.Rat, price1Float, price0Float, weight float64, amount0, amount1, liquidity, fee *big.Int, tick int64, ts time.Time) (onceStatus, bool) {
	var status onceStatus
	c.poolsMu.Lock()
	defer c.poolsMu.Unlock()

	pool, ok := c.pools[poolID]
	if !ok {
		return status, false
	}

	if canonicalPrice != nil {
		pool.lastPrice = new(big.Rat).Set(canonicalPrice)
	} else {
		pool.lastPrice = nil
	}
	if price1Per0 != nil {
		pool.lastPrice1Per0 = new(big.Rat).Set(price1Per0)
	} else {
		pool.lastPrice1Per0 = nil
	}
	if price0Per1 != nil {
		pool.lastPrice0Per1 = new(big.Rat).Set(price0Per1)
	} else {
		pool.lastPrice0Per1 = nil
	}
	pool.lastFloat1Per0 = price1Float
	pool.lastFloat0Per1 = price0Float
	pool.lastWeight = weight
	pool.lastAmount0 = cloneBig(amount0)
	pool.lastAmount1 = cloneBig(amount1)
	pool.lastLiquidity = cloneBig(liquidity)
	pool.lastTick = tick
	pool.lastFee = cloneBig(fee)
	pool.lastUpdate = ts

	if !pool.registered {
		pool.registered = true
	}

	if c.cfg.Once && !pool.onceEmitted {
		pool.onceEmitted = true
		status.marked = true
		for _, other := range c.pools {
			if !other.onceEmitted {
				status.remaining++
			}
		}
		status.completed = status.remaining == 0
	}

	return status, true
}

func (c *V4Connector) handleSwapEvent(ctx context.Context, evt abi.Event, logItem LogEvent, out chan<- *pb.MarketData) error {
	_ = ctx
	if len(logItem.Topics) < 2 {
		util.Debugf("uniswap_v4: swap missing pool topic tx=%s", logItem.TransactionHash)
		return nil
	}
	poolID := common.HexToHash(logItem.Topics[1])

	var (
		meta       V4PoolConfig
		registered bool
	)
	c.poolsMu.RLock()
	state, ok := c.pools[poolID]
	if ok {
		meta = state.meta
		registered = state.registered
	}
	c.poolsMu.RUnlock()
	if !ok {
		totalUnknown := atomic.AddUint64(&c.unknownPoolEvents, 1)
		if totalUnknown <= 5 || totalUnknown%100 == 0 {
			util.Infof("uniswap_v4: swap for unknown pool id=%s total_unknown=%d", poolID.Hex(), totalUnknown)
		} else {
			util.Debugf("uniswap_v4: swap for unknown pool id=%s total_unknown=%d", poolID.Hex(), totalUnknown)
		}
		return nil
	}

	dataField := strings.TrimSpace(logItem.Data)
	if dataField == "" || strings.EqualFold(dataField, "0x") {
		util.Debugf("uniswap_v4: swap data empty pair=%s", meta.PairName)
		return nil
	}

	payload, err := hexutil.Decode(dataField)
	if err != nil {
		util.Errorf("uniswap_v4: swap decode data pair=%s err=%v", meta.PairName, err)
		return nil
	}

	values := make(map[string]interface{})
	if err := evt.Inputs.NonIndexed().UnpackIntoMap(values, payload); err != nil {
		util.Errorf("uniswap_v4: swap unpack pair=%s err=%v", meta.PairName, err)
		return nil
	}

	sqrtPrice, ok := bigIntField(values, "sqrtPriceX96")
	if !ok {
		util.Debugf("uniswap_v4: swap missing sqrtPrice pair=%s", meta.PairName)
		return nil
	}

	price1Per0, price0Per1 := sqrtPriceToDirectionalPrices(meta, sqrtPrice)
	if price1Per0 == nil || price0Per1 == nil {
		util.Debugf("uniswap_v4: swap price nil pair=%s", meta.PairName)
		return nil
	}
	canonicalPrice := price1Per0
	if !meta.BaseIsToken0 {
		canonicalPrice = price0Per1
	}
	price1Float, price1Valid, price1Reason := ratToFloat64(price1Per0)
	price0Float, price0Valid, price0Reason := ratToFloat64(price0Per1)
	if !price1Valid {
		if price1Reason != "" {
			totalErr := atomic.AddUint64(&c.conversionErrors, 1)
			util.Errorf("uniswap_v4: price conversion error pair=%s dir=token1per0 reason=%s total_errors=%d", meta.PairName, price1Reason, totalErr)
		}
		price1Float = 0
	}
	if !price0Valid {
		if price0Reason != "" {
			totalErr := atomic.AddUint64(&c.conversionErrors, 1)
			util.Errorf("uniswap_v4: price conversion error pair=%s dir=token0per1 reason=%s total_errors=%d", meta.PairName, price0Reason, totalErr)
		}
		price0Float = 0
	}

	amount0, _ := bigIntField(values, "amount0")
	amount1, _ := bigIntField(values, "amount1")
	liquidity, _ := bigIntField(values, "liquidity")
	tickBI, _ := bigIntField(values, "tick")
	feeBI, _ := bigIntField(values, "fee")

	weight := calcSwapWeight(meta, amount0, amount1)
	if weight <= 0 {
		weight = 1e-9
	}

	var tickVal int64
	if tickBI != nil {
		tickVal = tickBI.Int64()
	}

	now := time.Now()
	status, ok := c.storeSwapMetrics(poolID, canonicalPrice, price1Per0, price0Per1, price1Float, price0Float, weight, amount0, amount1, liquidity, feeBI, tickVal, now)
	if !ok {
		util.Debugf("uniswap_v4: pool state missing during swap update pair=%s", meta.PairName)
		return nil
	}

	if !registered {
		c.registerToken(meta.Token0)
		c.registerToken(meta.Token1)
	}

	priceCanonStr := ratToString(canonicalPrice, 8)
	price1Str := ratToString(price1Per0, 8)
	price0Str := ratToString(price0Per1, 8)
	price1FloatStr := "n/a"
	price0FloatStr := "n/a"
	if price1Valid {
		price1FloatStr = fmt.Sprintf("%.10f", price1Float)
	}
	if price0Valid {
		price0FloatStr = fmt.Sprintf("%.10f", price0Float)
	}

	if c.cfg.LogAllEvents {
		util.Infof(
			"uniswap_v4: swap pair=%s canon=%s p1per0=%s p1float=%s p0per1=%s p0float=%s weight=%.6f amount0=%s amount1=%s liquidity=%s tick=%d fee=%s",
			meta.PairName,
			priceCanonStr,
			price1Str,
			price1FloatStr,
			price0Str,
			price0FloatStr,
			weight,
			formatAmount(amount0, meta.Token0.Decimals),
			formatAmount(amount1, meta.Token1.Decimals),
			formatAmount(liquidity, 0),
			tickVal,
			formatAmount(feeBI, 0),
		)
	} else {
		util.Debugf(
			"uniswap_v4: swap pair=%s canon=%s weight=%.4f p1per0=%s p0per1=%s",
			meta.PairName,
			priceCanonStr,
			weight,
			price1Str,
			price0Str,
		)
	}

	c.updatePricing(meta, price1Float, price1Valid, price0Float, price0Valid, weight, now, out)

	if c.cfg.Once && status.marked {
		if status.completed {
			util.Infof("uniswap_v4: once complete after pair=%s", meta.PairName)
			return errOnceComplete
		}
		util.Infof("uniswap_v4: once progress pair=%s remaining=%d", meta.PairName, status.remaining)
	}

	return nil
}

func (c *V4Connector) updatePricing(meta V4PoolConfig, price1Float float64, price1Valid bool, price0Float float64, price0Valid bool, weight float64, ts time.Time, out chan<- *pb.MarketData) {
	if c.pricer == nil {
		return
	}
	if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
		weight = 1e-9
	}
	info0 := pricing.TokenInfo{Address: meta.Token0.Address, Symbol: meta.Token0.Symbol, Decimals: meta.Token0.Decimals}
	info1 := pricing.TokenInfo{Address: meta.Token1.Address, Symbol: meta.Token1.Symbol, Decimals: meta.Token1.Decimals}
	updated := false
	if price1Valid && price1Float > 0 {
		c.pricer.UpdatePair(info0, info1, price1Float, weight, ts)
		updated = true
	}
	if price0Valid && price0Float > 0 {
		c.pricer.UpdatePair(info1, info0, price0Float, weight, ts)
		updated = true
	}
	if !updated {
		return
	}
	total := atomic.AddUint64(&c.successUpdates, 1)
	if price1Valid && price1Float > 0 {
		c.emitSpot(out, meta.Token0, meta.Token1, price1Float, ts)
	}
	if price0Valid && price0Float > 0 {
		c.emitSpot(out, meta.Token1, meta.Token0, price0Float, ts)
	}
	c.emitUSD(out, info0, ts)
	c.emitUSD(out, info1, ts)
	if total <= 10 || total%200 == 0 {
		convErr := atomic.LoadUint64(&c.conversionErrors)
		unknown := atomic.LoadUint64(&c.unknownPoolEvents)
		util.Infof("uniswap_v4: metrics success=%d conversion_errors=%d unknown_pools=%d", total, convErr, unknown)
	}
	if c.cfg.LogAllEvents {
		util.Infof("uniswap_v4: pricing update pair=%s weight=%.6f p1=%.10f p0=%.10f success_count=%d", meta.PairName, weight, price1Float, price0Float, total)
	} else {
		util.Debugf("uniswap_v4: pricing update pair=%s weight=%.6f success_count=%d", meta.PairName, weight, total)
	}
}

func (c *V4Connector) emitUSD(out chan<- *pb.MarketData, info pricing.TokenInfo, ts time.Time) {
	if c.pricer == nil || out == nil {
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
	exchange := c.exchangeName()
	rawSymbol := symbol + "USD"
	marketSymbol := util.NormalizeSpotSymbol(exchange, rawSymbol)
	if marketSymbol == "" {
		return
	}
	if ts.IsZero() {
		ts = time.Now()
	}
	md := &pb.MarketData{
		Exchange:  exchange,
		Symbol:    marketSymbol,
		Price:     res.Price,
		Timestamp: ts.UnixMilli(),
	}
	out <- md
	if c.cfg.LogAllEvents {
		upperRaw := strings.ToUpper(rawSymbol)
		if marketSymbol != upperRaw {
			util.Infof("uniswap_v4: usd symbol normalized raw=%s normalized=%s exchange=%s", upperRaw, marketSymbol, exchange)
		}
		route := strings.Join(res.Route, "->")
		if route == "" {
			route = "direct"
		}
		util.Infof("uniswap_v4: usd %s price=%.8f weight=%.6f route=%s", marketSymbol, res.Price, res.Weight, route)
	}
}
func parseAck(raw []byte) (*SubAck, bool) {
	var ack SubAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		return nil, false
	}
	if ack.Result == "" && ack.Error == nil {
		return nil, false
	}
	return &ack, true
}

func sqrtPriceToDirectionalPrices(pool V4PoolConfig, sqrt *big.Int) (*big.Rat, *big.Rat) {
	if sqrt == nil || sqrt.Sign() <= 0 {
		return nil, nil
	}
	squared := new(big.Int).Mul(sqrt, sqrt)
	price := new(big.Rat).SetFrac(squared, twoPow192)
	decAdj := new(big.Rat).SetFrac(pow10Int(pool.Token0.Decimals), pow10Int(pool.Token1.Decimals))
	price.Mul(price, decAdj)
	price1Per0 := price
	price0Per1 := invertRat(price1Per0)
	return price1Per0, price0Per1
}

func pow10Int(dec int) *big.Int {
	if dec <= 0 {
		return big.NewInt(1)
	}
	if cached, ok := pow10Cache.Load(dec); ok {
		return new(big.Int).Set(cached.(*big.Int))
	}
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil)
	pow10Cache.Store(dec, pow)
	return new(big.Int).Set(pow)
}

func ratToString(r *big.Rat, precision int) string {
	if r == nil {
		return ""
	}
	str := new(big.Float).SetPrec(256).SetRat(r).Text('f', precision)
	return str
}

func ratToFloat64(r *big.Rat) (float64, bool, string) {
	if r == nil {
		return 0, false, "nil"
	}
	if r.Sign() <= 0 {
		return 0, false, "non-positive-rational"
	}
	val, _ := new(big.Float).SetPrec(256).SetRat(r).Float64()
	if math.IsNaN(val) {
		return 0, false, "nan"
	}
	if math.IsInf(val, 0) {
		return 0, false, "inf"
	}
	if val <= 0 {
		return 0, false, "non-positive-float"
	}
	return val, true, ""
}

func invertRat(r *big.Rat) *big.Rat {
	if r == nil || r.Sign() == 0 {
		return nil
	}
	return new(big.Rat).Inv(r)
}

func bigIntField(fields map[string]interface{}, key string) (*big.Int, bool) {
	val, ok := fields[key]
	if !ok || val == nil {
		return nil, false
	}
	switch v := val.(type) {
	case *big.Int:
		return v, true
	case big.Int:
		return new(big.Int).Set(&v), true
	case *big.Rat:
		return new(big.Int).Set(v.Num()), true
	case string:
		if v == "" {
			return nil, false
		}
		if strings.HasPrefix(v, "0x") {
			bi, err := hexutil.DecodeBig(v)
			if err == nil {
				return bi, true
			}
		}
		if bi, ok := new(big.Int).SetString(v, 10); ok {
			return bi, true
		}
	case []byte:
		if len(v) == 0 {
			return nil, false
		}
		return new(big.Int).SetBytes(v), true
	case uint64:
		return new(big.Int).SetUint64(v), true
	case uint32:
		return new(big.Int).SetUint64(uint64(v)), true
	case uint16:
		return new(big.Int).SetUint64(uint64(v)), true
	case uint8:
		return new(big.Int).SetUint64(uint64(v)), true
	case int64:
		return big.NewInt(v), true
	case int32:
		return big.NewInt(int64(v)), true
	case int16:
		return big.NewInt(int64(v)), true
	case int8:
		return big.NewInt(int64(v)), true
	case int:
		return big.NewInt(int64(v)), true
	case uint:
		return new(big.Int).SetUint64(uint64(v)), true
	}
	return nil, false
}

func calcSwapWeight(pool V4PoolConfig, amount0, amount1 *big.Int) float64 {
	w0 := amountToFloat(amount0, pool.Token0.Decimals)
	w1 := amountToFloat(amount1, pool.Token1.Decimals)
	weight := math.Max(w0, w1)
	if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
		return 0
	}
	return weight
}

func amountToFloat(amount *big.Int, decimals int) float64 {
	if amount == nil {
		return 0
	}
	abs := new(big.Int).Abs(new(big.Int).Set(amount))
	if abs.Sign() == 0 {
		return 0
	}
	f := new(big.Float).SetPrec(256).SetInt(abs)
	if decimals > 0 {
		den := new(big.Float).SetPrec(256).SetInt(pow10Int(decimals))
		f.Quo(f, den)
	}
	val, _ := f.Float64()
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return 0
	}
	return val
}

func formatAmount(amount *big.Int, decimals int) string {
	if amount == nil {
		return ""
	}
	val := amountToFloat(amount, decimals)
	sign := ""
	if amount.Sign() < 0 {
		sign = "-"
	}
	if val == 0 {
		return "0"
	}
	return fmt.Sprintf("%s%.6f", sign, val)
}

func cloneBig(src *big.Int) *big.Int {
	if src == nil {
		return nil
	}
	return new(big.Int).Set(src)
}
func parseNote(raw []byte) (*SubNote, bool) {
	var note SubNote
	if err := json.Unmarshal(raw, &note); err != nil {
		return nil, false
	}
	if !strings.EqualFold(note.Method, "eth_subscription") {
		return nil, false
	}
	return &note, true
}

func (c *V4Connector) handleLog(ctx context.Context, event LogEvent, out chan<- *pb.MarketData) error {
	_ = out
	if event.Removed {
		util.Debugf("uniswap_v4: skip removed log index=%s", event.LogIndex)
		return nil
	}
	if err := c.ensureABI(); err != nil {
		return err
	}
	if len(event.Topics) == 0 {
		util.Debugf("uniswap_v4: log without topics hash=%s", event.TransactionHash)
		return nil
	}
	sig := common.HexToHash(event.Topics[0])
	evt, ok := c.eventBySig[sig]
	if !ok {
		util.Debugf("uniswap_v4: unknown event sig=%s topics=%d", sig.Hex(), len(event.Topics))
		return nil
	}

	switch evt.Name {
	case "Swap":
		return c.handleSwapEvent(ctx, evt, event, out)
	default:
		if c.cfg.LogAllEvents {
			util.Debugf("uniswap_v4: event=%s topics=%d data_len=%d", evt.Name, len(event.Topics), len(event.Data))
		}
		return nil
	}
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoffDuration(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		next = max
	}
	if next < 2*time.Second {
		next = 2 * time.Second
	}
	return next
}
