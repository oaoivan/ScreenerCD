package uniswap

// Подписка на ВСЕ Uniswap V3 пулы (ETH сеть) из base_pools.json одним WS подключением Alchemy.
// Цель: стрим всех Swap/Sync-подобных событий (в V3 – Swap) для проверки формулы цены.
// Минимальная версия: логирует sqrtPriceX96 и выводит price(token1/token0) и inverse без нормализации по decimals (если ещё не загружены).
// При первом событии пула — лениво подтягиваем token0/token1 metadata (symbol, decimals) через eth_call по тому же WS.
// Ограничений по времени / количеству событий нет — останавливать Ctrl+C.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
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
	core "github.com/yourusername/screner/internal/dex"
	"github.com/yourusername/screner/internal/dex/pricing"
	"github.com/yourusername/screner/internal/util"
	pb "github.com/yourusername/screner/pkg/protobuf"
)

// --- Константы ---
const (
	v3DefaultMainnetTemplate = "wss://eth-mainnet.g.alchemy.com/v2/%s"
	v3GeckoDefaultPath       = ""
	v3BatchSizeDefault       = 150
	v3PingIntervalDefault    = 25 * time.Second
	v3ReconnectBase          = 2 * time.Second
	v3ReconnectMax           = 30 * time.Second
	v3DefaultPoolABIPath     = "ABI/Uniswap/V3/UniswapV3Pool.json"
)

// --- Структуры входного файла GeckoTerminal ---
type v3IntOrString int

func (v *v3IntOrString) UnmarshalJSON(b []byte) error {
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
		*v = v3IntOrString(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(bb, &n); err != nil {
		return err
	}
	*v = v3IntOrString(n)
	return nil
}

type v3GeckoPool struct {
	AMMVersion  string `json:"amm_version"`
	Dex         string `json:"dex"`
	PairName    string `json:"pair_name"`
	PoolID      string `json:"pool_id"`
	PoolAddress string `json:"pool_address"`
	Network     string `json:"network"`
	Token0      struct {
		Address  string        `json:"address"`
		Symbol   string        `json:"symbol"`
		Decimals v3IntOrString `json:"decimals"`
	} `json:"token0"`
	Token1 struct {
		Address  string        `json:"address"`
		Symbol   string        `json:"symbol"`
		Decimals v3IntOrString `json:"decimals"`
	} `json:"token1"`
}

type v3GeckoFile struct {
	Entries []v3GeckoPool `json:"entries"`
}

func v3TokenMetaFromJSON(address, symbol string, decimals v3IntOrString) v3TokenMeta {
	meta := v3TokenMeta{}
	addr := strings.TrimSpace(address)
	if common.IsHexAddress(addr) {
		meta.Address = common.HexToAddress(addr)
	}
	sym := strings.TrimSpace(symbol)
	if sym != "" {
		meta.Symbol = strings.ToUpper(sym)
	}
	dec := int(decimals)
	if dec < 0 {
		dec = 0
	}
	if dec > 255 {
		dec = 255
	}
	meta.Dec = uint8(dec)
	return meta
}

func v3DefaultSymbol(addr common.Address) string {
	hex := strings.ToUpper(strings.TrimPrefix(addr.Hex(), "0x"))
	if len(hex) <= 6 {
		return hex
	}
	return hex[:3] + hex[len(hex)-3:]
}

// --- Подписка / RPC ---
type v3RPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type v3SubAck struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  string `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type v3SubNote struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		Subscription string    `json:"subscription"`
		Result       v3LogItem `json:"result"`
	} `json:"params"`
}

type v3LogItem struct {
	Address         string   `json:"address"`
	Data            string   `json:"data"`
	Topics          []string `json:"topics"`
	BlockNumber     string   `json:"blockNumber"`
	TransactionHash string   `json:"transactionHash"`
	Removed         bool     `json:"removed"`
}

// --- Мета пула ---
type v3TokenMeta struct {
	Address common.Address
	Symbol  string
	Dec     uint8
}

type v3PoolMeta struct {
	Addr         common.Address
	PairName     string
	Dex          string
	AMMVersion   string
	Network      string
	Token0       v3TokenMeta
	Token1       v3TokenMeta
	Loaded       bool // метаданные токенов загружены
	Loading      bool
	LoadErr      error
	HasJSONMeta  bool
	Registered   bool
	Verified     bool
	Descriptor   core.PoolDescriptor
	CompositeKey string
}

const v3FallbackWETHHex = "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"

var v3FallbackStableSymbols = []string{"USDC", "USDT", "DAI", "TUSD", "FDUSD"}

type v3Engine struct {
	ctx context.Context
	cfg V3Config

	exchange  string
	network   string
	chainID   uint64
	logPrefix string

	pricer pricing.Pricer

	abi        abi.ABI
	eventBySig map[common.Hash]abi.Event
	twoPow192  *big.Int

	pools      map[common.Address]*v3PoolMeta
	tokenCache map[common.Address]v3TokenMeta
	tokenMu    sync.RWMutex
	pow10Cache map[uint8]*big.Int
	pow10Mu    sync.RWMutex

	httpClient *http.Client
	wsURL      string
	httpURL    string

	pingInterval   time.Duration
	batchSize      int
	stopOnAckErr   bool
	logAllEvents   bool
	decodeSwapOnly bool

	poolABIPath string

	out chan<- *pb.MarketData

	registry     *TokenRegistry
	stableLookup map[string]struct{}
	wethAddress  common.Address
	wethUSD      *big.Rat
	wethStable   string
	wethMu       sync.RWMutex

	metaSem chan struct{}

	messageCount   uint64
	reconnectCount uint64
}

func newV3Engine(ctx context.Context, cfg V3Config, pr pricing.Pricer, out chan<- *pb.MarketData) (*v3Engine, error) {
	if out == nil {
		return nil, fmt.Errorf("uniswap_v3: out channel is nil")
	}

	poolABI := strings.TrimSpace(cfg.PoolABIPath)
	e := &v3Engine{
		ctx:          ctx,
		cfg:          cfg,
		pricer:       pr,
		out:          out,
		eventBySig:   make(map[common.Hash]abi.Event),
		twoPow192:    new(big.Int).Lsh(big.NewInt(1), 192),
		pools:        make(map[common.Address]*v3PoolMeta),
		tokenCache:   make(map[common.Address]v3TokenMeta),
		pow10Cache:   make(map[uint8]*big.Int),
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		stableLookup: v3FallbackStableSet(),
		poolABIPath:  poolABI,
	}

	if cfg.MaxMetaWorkers > 0 {
		e.metaSem = make(chan struct{}, cfg.MaxMetaWorkers)
	} else {
		e.metaSem = make(chan struct{}, 8)
	}

	if cfg.BatchSize > 0 {
		e.batchSize = cfg.BatchSize
	} else {
		e.batchSize = v3BatchSizeDefault
	}

	if cfg.PingInterval > 0 {
		e.pingInterval = cfg.PingInterval
	} else {
		e.pingInterval = v3PingIntervalDefault
	}

	e.stopOnAckErr = cfg.StopOnAckError
	e.logAllEvents = cfg.LogAllEvents
	e.decodeSwapOnly = cfg.DecodeSwapOnly

	name := strings.TrimSpace(cfg.Exchange)
	if name == "" {
		name = "uniswap_v3"
	}
	e.exchange = strings.ToLower(name)

	networkName := strings.TrimSpace(cfg.Network)
	if networkName == "" && cfg.Registry != nil {
		networkName = cfg.Registry.NetworkName()
	}
	e.network = strings.ToLower(networkName)

	chainID := cfg.ChainID
	if chainID == 0 && cfg.Registry != nil {
		chainID = cfg.Registry.ChainID()
	}
	e.chainID = chainID

	if e.network != "" {
		e.logPrefix = fmt.Sprintf("uniswap_v3:%s", e.network)
	} else {
		e.logPrefix = "uniswap_v3"
	}

	if e.poolABIPath == "" {
		e.poolABIPath = v3DefaultPoolABIPath
	}
	util.Infof("%s engine pool abi=%s", e.logPrefix, e.poolABIPath)
	return e, nil
}

func v3FallbackStableSet() map[string]struct{} {
	lookup := make(map[string]struct{}, len(v3FallbackStableSymbols))
	for _, sym := range v3FallbackStableSymbols {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			continue
		}
		lookup[key] = struct{}{}
	}
	return lookup
}

func (e *v3Engine) initAssets() {
	reg := e.cfg.Registry
	provided := reg != nil
	if reg == nil {
		util.Debugf("%s assets: registry nil, using defaults", e.logPrefix)
		reg = NewTokenRegistry(nil, RegistryOptions{})
	}
	e.registry = reg

	lookup := v3FallbackStableSet()
	added := 0
	for _, meta := range reg.StableTokens() {
		key := strings.ToUpper(strings.TrimSpace(meta.Symbol))
		if key == "" {
			continue
		}
		if _, exists := lookup[key]; !exists {
			added++
		}
		lookup[key] = struct{}{}
	}

	if added > 0 {
		util.Infof("%s assets: registry stables added=%d total=%d", e.logPrefix, added, len(lookup))
	} else if provided {
		util.Debugf("%s assets: registry stables empty, fallback total=%d", e.logPrefix, len(lookup))
	} else {
		util.Debugf("%s assets: fallback stables total=%d", e.logPrefix, len(lookup))
	}
	e.stableLookup = lookup

	addr := common.Address{}
	if wethMeta, ok := reg.WETHMeta(); ok && (wethMeta.Address != common.Address{}) {
		addr = wethMeta.Address
	}
	if (addr == common.Address{}) {
		addr = reg.WETHAddress()
	}
	if (addr == common.Address{}) {
		addr = common.HexToAddress(v3FallbackWETHHex)
		if provided {
			util.Debugf("%s assets: registry missing WETH, fallback address=%s", e.logPrefix, addr.Hex())
		} else {
			util.Debugf("%s assets: WETH fallback address=%s", e.logPrefix, addr.Hex())
		}
	} else {
		util.Infof("%s assets: WETH address=%s", e.logPrefix, addr.Hex())
	}
	e.wethAddress = addr
}

func (e *v3Engine) isStableSymbol(symbol string) bool {
	key := strings.ToUpper(strings.TrimSpace(symbol))
	if key == "" {
		return false
	}
	if e.registry != nil && e.registry.IsStableSymbol(key) {
		return true
	}
	if e.stableLookup != nil {
		if _, ok := e.stableLookup[key]; ok {
			return true
		}
	}
	if _, ok := v3FallbackStableSet()[key]; ok {
		return true
	}
	return false
}

type V3Config struct {
	Exchange        string
	Network         string
	ChainID         uint64
	WSURL           string
	HTTPURL         string
	PoolsPath       string
	DexFilter       string
	DexFilters      []string
	NetworkFilter   string
	NetworkFilters  []string
	BatchSize       int
	PingInterval    time.Duration
	StopOnAckError  bool
	LogAllEvents    bool
	DecodeSwapOnly  bool
	MaxMetaWorkers  int
	Registry        *TokenRegistry
	WantedPairs     []string
	AMMVersions     []string
	IdentityFilters []string
	PoolABIPath     string
}

type V3Connector struct {
	cfg    V3Config
	pricer pricing.Pricer
}

func NewV3Connector(cfg V3Config, pricer pricing.Pricer) *V3Connector {
	return &V3Connector{cfg: cfg, pricer: pricer}
}

func (c *V3Connector) Run(ctx context.Context, out chan<- *pb.MarketData) error {
	engine, err := newV3Engine(ctx, c.cfg, c.pricer, out)
	if err != nil {
		return err
	}
	return engine.Run()
}

// Event сигнатуры V3 (минимум Swap). Keccak("Swap(address,address,int256,int256,uint160,uint128,int24)")
var v3SwapSig = common.HexToHash("0xc42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67")

func (e *v3Engine) Run() error {
	if err := e.initABI(); err != nil {
		return fmt.Errorf("uniswap_v3: init abi: %w", err)
	}
	if err := e.resolveWS(); err != nil {
		return err
	}
	if err := e.loadPools(); err != nil {
		return err
	}
	if len(e.pools) == 0 {
		return fmt.Errorf("uniswap_v3: no pools loaded")
	}
	e.initAssets()

	atomic.StoreUint64(&e.messageCount, 0)
	atomic.StoreUint64(&e.reconnectCount, 0)
	defer func() {
		emitted := atomic.LoadUint64(&e.messageCount)
		reconnects := atomic.LoadUint64(&e.reconnectCount)
		util.Infof("%s connector stopped, emitted=%d reconnects=%d", e.logPrefix, emitted, reconnects)
	}()

	util.Infof("%s start exchange=%s network=%s chain_id=%d pools=%d ws=%s", e.logPrefix, e.exchange, e.network, e.chainID, len(e.pools), v3Mask(e.wsURL))
	return e.runLoop()
}

// --- Init / Load ---
func (e *v3Engine) initABI() error {
	path := strings.TrimSpace(e.poolABIPath)
	if path == "" {
		path = v3DefaultPoolABIPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("uniswap_v3: read abi %s: %w", path, err)
	}
	parsed, err := abi.JSON(bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("uniswap_v3: parse abi %s: %w", path, err)
	}
	e.abi = parsed
	e.eventBySig = make(map[common.Hash]abi.Event, len(parsed.Events))
	for _, ev := range e.abi.Events {
		e.eventBySig[ev.ID] = ev
	}
	util.Infof("%s abi loaded path=%s events=%d", e.logPrefix, path, len(parsed.Events))
	return nil
}

func (e *v3Engine) resolveWS() error {
	ws := strings.TrimSpace(e.cfg.WSURL)
	httpURL := strings.TrimSpace(e.cfg.HTTPURL)

	if ws == "" {
		if direct := strings.TrimSpace(os.Getenv("ALCHEMY_WS_URL")); direct != "" {
			ws = direct
		} else {
			key := strings.TrimSpace(os.Getenv("ALCHEMY_API_KEY"))
			if key == "" {
				return fmt.Errorf("uniswap_v3: ws_url not provided (set ws_url or ALCHEMY_API_KEY)")
			}
			ws = fmt.Sprintf(v3DefaultMainnetTemplate, key)
			if httpURL == "" {
				httpURL = fmt.Sprintf("https://eth-mainnet.g.alchemy.com/v2/%s", key)
			}
		}
	}

	if httpURL == "" {
		if fromEnv := strings.TrimSpace(os.Getenv("ALCHEMY_HTTP_URL")); fromEnv != "" {
			httpURL = fromEnv
		} else if strings.HasPrefix(ws, "wss://") {
			httpURL = "https://" + strings.TrimPrefix(ws, "wss://")
		}
	}

	if httpURL == "" {
		return fmt.Errorf("uniswap_v3: http_url not provided")
	}

	e.wsURL = ws
	e.httpURL = httpURL
	return nil
}

func (e *v3Engine) loadPools() error {
	path := strings.TrimSpace(e.cfg.PoolsPath)
	if path == "" {
		path = strings.TrimSpace(os.Getenv("GECKO_POOLS_JSON"))
	}
	if path == "" {
		path = v3GeckoDefaultPath
	}
	if path == "" {
		return fmt.Errorf("uniswap_v3: pools path not provided")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var f v3GeckoFile
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	networkFilters := e.cfg.NetworkFilters
	if len(networkFilters) == 0 {
		trimmed := strings.TrimSpace(e.cfg.NetworkFilter)
		if trimmed != "" {
			networkFilters = []string{trimmed}
		}
	}
	ammFilters := e.cfg.AMMVersions
	dexFilters := e.cfg.DexFilters
	allowedIdentities := make(map[string]struct{}, len(e.cfg.IdentityFilters))
	for _, key := range e.cfg.IdentityFilters {
		norm := strings.ToLower(strings.TrimSpace(key))
		if norm == "" {
			continue
		}
		allowedIdentities[norm] = struct{}{}
	}

	e.pools = make(map[common.Address]*v3PoolMeta)
	seenKeys := make(map[string]struct{})
	added := 0
	for _, entry := range f.Entries {
		if !matchesAnyAMMVersion(entry.AMMVersion, ammFilters) {
			continue
		}
		dexName := strings.TrimSpace(entry.Dex)
		if dexName == "" && len(dexFilters) == 1 {
			dexName = dexFilters[0]
		}
		if len(dexFilters) > 0 && !dexMatchesAny(dexName, dexFilters) {
			continue
		}
		if len(networkFilters) > 0 && !networkMatchesAny(entry.Network, networkFilters) {
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
		if _, exists := e.pools[addr]; exists {
			continue
		}
		token0 := v3TokenMetaFromJSON(entry.Token0.Address, entry.Token0.Symbol, entry.Token0.Decimals)
		token1 := v3TokenMetaFromJSON(entry.Token1.Address, entry.Token1.Symbol, entry.Token1.Decimals)
		if (token0.Address == common.Address{}) || (token1.Address == common.Address{}) {
			util.Debugf("%s skip pool=%s reason=missing token address", e.logPrefix, entry.PairName)
			continue
		}
		if token0.Symbol == "" {
			token0.Symbol = v3DefaultSymbol(token0.Address)
		}
		if token1.Symbol == "" {
			token1.Symbol = v3DefaultSymbol(token1.Address)
		}
		hasJSONMeta := token0.Dec > 0 && token1.Dec > 0
		meta := &v3PoolMeta{
			Addr:        addr,
			PairName:    entry.PairName,
			Dex:         dexName,
			AMMVersion:  entry.AMMVersion,
			Network:     entry.Network,
			Token0:      token0,
			Token1:      token1,
			HasJSONMeta: hasJSONMeta,
		}
		if err := e.attachDescriptor(meta); err != nil {
			util.Errorf("%s skip pool=%s descriptor err=%v", e.logPrefix, entry.PairName, err)
			continue
		}
		if len(allowedIdentities) > 0 {
			key := strings.ToLower(identityKey(meta.Dex, meta.AMMVersion, meta.Network))
			if key == "" {
				util.Debugf("%s skip pool=%s reason=empty identity", e.logPrefix, entry.PairName)
				continue
			}
			if _, ok := allowedIdentities[key]; !ok {
				continue
			}
		}
		if meta.CompositeKey == "" {
			util.Errorf("%s skip pool=%s reason=empty composite key", e.logPrefix, entry.PairName)
			continue
		}
		if _, dup := seenKeys[meta.CompositeKey]; dup {
			util.Debugf("%s skip duplicate composite key=%s pair=%s", e.logPrefix, meta.CompositeKey, entry.PairName)
			continue
		}
		seenKeys[meta.CompositeKey] = struct{}{}
		e.pools[addr] = meta
		if hasJSONMeta {
			util.Debugf("%s pool=%s decimals json token0=%d token1=%d", e.logPrefix, entry.PairName, token0.Dec, token1.Dec)
		}
		added++
		if added <= 20 {
			util.Infof("%s add pool %s addr=%s", e.logPrefix, entry.PairName, addr.Hex())
		}
	}
	util.Infof("%s loaded pools=%d source=%s", e.logPrefix, added, path)
	return nil
}

func (e *v3Engine) attachDescriptor(meta *v3PoolMeta) error {
	dexName := strings.TrimSpace(meta.Dex)
	if dexName == "" {
		dexName = e.exchange
	}
	dexName = normalizeDexName(dexName)
	if dexName == "" {
		return fmt.Errorf("pool descriptor: dex is empty")
	}
	amm := strings.TrimSpace(meta.AMMVersion)
	if amm == "" {
		if len(e.cfg.AMMVersions) > 0 {
			amm = e.cfg.AMMVersions[0]
		} else {
			amm = "v3"
		}
	}
	amm = normalizeAMMVersion(amm)
	if amm == "" {
		return fmt.Errorf("pool descriptor: amm_version is empty")
	}
	network := strings.TrimSpace(meta.Network)
	if network == "" {
		network = e.network
	}
	network = normalizeNetworkName(network)
	if network == "" {
		return fmt.Errorf("pool descriptor: network is empty")
	}
	desc := core.PoolDescriptor{
		Dex:        dexName,
		AMMVersion: amm,
		Network:    network,
		Token0: core.PoolToken{
			Address: meta.Token0.Address,
			Symbol:  meta.Token0.Symbol,
		},
		Token1: core.PoolToken{
			Address: meta.Token1.Address,
			Symbol:  meta.Token1.Symbol,
		},
	}
	if err := desc.Validate(); err != nil {
		return err
	}
	meta.Dex = desc.Dex
	meta.AMMVersion = desc.AMMVersion
	meta.Network = desc.Network
	meta.Descriptor = desc
	meta.CompositeKey = desc.CompositeKey()
	util.Debugf("%s descriptor attached pair=%s key=%s", e.logPrefix, meta.PairName, meta.CompositeKey)
	return nil
}

// --- Run Loop ---
func (e *v3Engine) runLoop() error {
	backoff := v3ReconnectBase
	for {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		conn, err := e.dial()
		if err != nil {
			util.Errorf("%s dial error: %v", e.logPrefix, err)
			backoff = v3NextBackoff(backoff)
			select {
			case <-e.ctx.Done():
				return e.ctx.Err()
			case <-time.After(backoff):
			}
			continue
		}
		util.Infof("%s ws connected %s", e.logPrefix, v3Mask(e.wsURL))
		if err := e.subscribeAll(conn); err != nil {
			util.Errorf("%s subscribe error: %v", e.logPrefix, err)
			conn.Close()
			backoff = v3NextBackoff(backoff)
			select {
			case <-e.ctx.Done():
				return e.ctx.Err()
			case <-time.After(backoff):
			}
			continue
		}
		backoff = v3ReconnectBase
		loopCtx, cancel := context.WithCancel(e.ctx)
		msgs := make(chan []byte, 4096)
		errs := make(chan error, 1)
		go e.ping(loopCtx, conn)
		go e.read(conn, msgs, errs)
		running := true
		for running {
			select {
			case <-e.ctx.Done():
				cancel()
				conn.Close()
				return e.ctx.Err()
			case raw, ok := <-msgs:
				if !ok {
					util.Infof("%s reader closed -> reconnect", e.logPrefix)
					running = false
					continue
				}
				e.handle(raw)
			case err := <-errs:
				if err != nil {
					util.Errorf("%s ws error: %v", e.logPrefix, err)
				}
				running = false
			}
		}
		cancel()
		conn.Close()
		select {
		case <-e.ctx.Done():
			return e.ctx.Err()
		case <-time.After(backoff):
		}
		count := atomic.AddUint64(&e.reconnectCount, 1)
		util.Infof("%s reconnect #%d in %s", e.logPrefix, count, backoff)
		backoff = v3NextBackoff(backoff)
	}
}

func (e *v3Engine) dial() (*websocket.Conn, error) {
	d := *websocket.DefaultDialer
	d.EnableCompression = true
	c, _, err := d.Dial(e.wsURL, nil)
	return c, err
}

func (e *v3Engine) subscribeAll(conn *websocket.Conn) error {
	addresses := make([]string, 0, len(e.pools))
	for a := range e.pools {
		addresses = append(addresses, a.Hex())
	}
	sort.Strings(addresses)
	batches := 0
	id := 1
	for start := 0; start < len(addresses); start += e.batchSize {
		end := start + e.batchSize
		if end > len(addresses) {
			end = len(addresses)
		}
		part := addresses[start:end]
		req := v3RPCRequest{JSONRPC: "2.0", ID: id, Method: "eth_subscribe", Params: []interface{}{"logs", map[string]interface{}{"address": part}}}
		if err := conn.WriteJSON(req); err != nil {
			return fmt.Errorf("batch %d: %w", batches, err)
		}
		util.Debugf("%s subscribe batch=%d id=%d size=%d", e.logPrefix, batches, id, len(part))
		batches++
		id++
	}
	util.Infof("%s total subscribe batches=%d", e.logPrefix, batches)
	return nil
}

// --- WS helpers ---
func (e *v3Engine) ping(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(e.pingInterval)
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

func (e *v3Engine) read(c *websocket.Conn, out chan<- []byte, errs chan<- error) {
	defer close(out)
	for {
		mt, data, err := c.ReadMessage()
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
func v3NextBackoff(cur time.Duration) time.Duration {
	n := cur * 2
	if n > v3ReconnectMax {
		return v3ReconnectMax
	}
	return n
}

// --- Message handling ---
func (e *v3Engine) handle(raw []byte) {
	if ack, ok := v3TryAck(raw); ok {
		if ack.Error != nil {
			util.Errorf("%s ack error code=%d msg=%s", e.logPrefix, ack.Error.Code, ack.Error.Message)
			if e.stopOnAckErr {
				util.Fatalf("%s ack error stop", e.logPrefix)
			}
		} else {
			util.Infof("%s subscribed id=%s", e.logPrefix, ack.Result)
		}
		return
	}
	note, ok := v3TryNote(raw)
	if !ok {
		return
	}
	e.process(note.Params.Result)
}

func v3TryAck(raw []byte) (*v3SubAck, bool) {
	var a v3SubAck
	if json.Unmarshal(raw, &a) != nil {
		return nil, false
	}
	if a.Result == "" && a.Error == nil {
		return nil, false
	}
	return &a, true
}
func v3TryNote(raw []byte) (*v3SubNote, bool) {
	var n v3SubNote
	if json.Unmarshal(raw, &n) != nil {
		return nil, false
	}
	if !strings.EqualFold(n.Method, "eth_subscription") {
		return nil, false
	}
	return &n, true
}

// --- Event decoding ---
func (e *v3Engine) process(l v3LogItem) {
	if l.Removed || len(l.Topics) == 0 {
		return
	}
	sig := common.HexToHash(l.Topics[0])
	if e.decodeSwapOnly && sig != v3SwapSig {
		return
	}
	addr := common.HexToAddress(l.Address)
	pm, ok := e.pools[addr]
	if !ok {
		return
	}
	if sig == v3SwapSig {
		e.handleSwap(pm, l)
	} else if e.logAllEvents {
		util.Infof("%s evt addr=%s topic=%s tx=%s", e.logPrefix, addr.Hex(), sig.Hex(), l.TransactionHash)
	}
}

func (e *v3Engine) handleSwap(pm *v3PoolMeta, l v3LogItem) {
	if len(l.Data) < 2 {
		return
	}
	dataBytes, err := hexutil.Decode(l.Data)
	if err != nil {
		return
	}
	if len(dataBytes) < 32*5 {
		return
	}
	amount0 := v3DecodeInt256(dataBytes[0:32])
	amount1 := v3DecodeInt256(dataBytes[32:64])
	sqrtPriceX96 := new(big.Int).SetBytes(dataBytes[64:96])

	rawPrice := e.priceFromSqrt(sqrtPriceX96)
	if rawPrice == nil {
		return
	}

	if !pm.Verified {
		if e.tryAcquireMetaSlot() {
			if err := e.ensurePoolMeta(pm); err != nil {
				pm.LoadErr = err
				util.Errorf("%s meta pool=%s err=%v", e.logPrefix, pm.Addr.Hex(), err)
			} else {
				pm.Loaded = true
				pm.Verified = true
			}
			e.releaseMetaSlot()
		}
	}
	if (pm.HasJSONMeta || pm.Loaded) && !pm.Registered {
		e.registerToken(pm.Token0)
		e.registerToken(pm.Token1)
		pm.Registered = true
	}

	var price1Per0, price0Per1 *big.Rat
	var priceNote string
	if (pm.Loaded || pm.HasJSONMeta) && pm.Token0.Dec > 0 && pm.Token1.Dec > 0 {
		adj := e.decimalAdjust(pm.Token0.Dec, pm.Token1.Dec)
		price1Per0 = new(big.Rat).Mul(rawPrice, adj)
		price0Per1 = v3Invert(price1Per0)
		priceNote = "norm"
	} else {
		price1Per0 = rawPrice
		price0Per1 = v3Invert(rawPrice)
		priceNote = "raw"
	}

	usdLines := e.deriveUSD(pm, price1Per0, price0Per1)

	if e.logAllEvents {
		util.Infof("%s swap pool=%s addr=%s sqrtP=%s mode=%s p1per0=%s p0per1=%s amt0=%s amt1=%s blk=%s tx=%s %s",
			e.logPrefix, pm.PairName, pm.Addr.Hex(), sqrtPriceX96.String(), priceNote, v3Format(price1Per0, 8), v3Format(price0Per1, 8), amount0.String(), amount1.String(), l.BlockNumber, l.TransactionHash, usdLines)
	}

	e.emitPair(pm, pm.Token0.Symbol, pm.Token1.Symbol, price1Per0)
	e.emitPair(pm, pm.Token1.Symbol, pm.Token0.Symbol, price0Per1)
	e.updatePricing(pm, price1Per0, price0Per1, amount0, amount1)
}

// --- Decoding helpers ---
func v3DecodeInt256(b []byte) *big.Int {
	if len(b) == 0 {
		return big.NewInt(0)
	}
	v := new(big.Int).SetBytes(b)
	if b[0]&0x80 != 0 { // отрицательное
		twoPow := new(big.Int).Lsh(big.NewInt(1), uint(8*len(b)))
		v.Sub(v, twoPow)
	}
	return v
}

func v3Invert(r *big.Rat) *big.Rat {
	if r == nil || r.Sign() == 0 {
		return nil
	}
	return new(big.Rat).Inv(r)
}
func (e *v3Engine) priceFromSqrt(s *big.Int) *big.Rat {
	if s == nil || s.Sign() == 0 {
		return nil
	}
	sq := new(big.Int).Mul(s, s)
	return new(big.Rat).SetFrac(sq, e.twoPow192)
}
func v3Format(r *big.Rat, prec int) string {
	if r == nil {
		return "?"
	}
	f := new(big.Float).SetPrec(256).SetRat(r)
	return f.Text('f', prec)
}

func (e *v3Engine) emitPair(pm *v3PoolMeta, base, quote string, value *big.Rat) {
	if value == nil {
		return
	}
	base = strings.ToUpper(strings.TrimSpace(base))
	quote = strings.ToUpper(strings.TrimSpace(quote))
	if base == "" || quote == "" {
		return
	}
	e.publish(pm, base+quote, value)
}

func (e *v3Engine) publish(pm *v3PoolMeta, symbol string, value *big.Rat) {
	if value == nil || e.out == nil {
		return
	}
	f64, _ := new(big.Float).SetPrec(256).SetRat(value).Float64()
	if f64 <= 0 || math.IsInf(f64, 0) || math.IsNaN(f64) {
		return
	}
	identity := e.poolIdentity(pm)
	md := &pb.MarketData{
		Exchange:   e.exchange,
		Symbol:     strings.ToUpper(strings.TrimSpace(symbol)),
		Price:      f64,
		Timestamp:  time.Now().UnixMilli(),
		Network:    util.NormalizeNetworkName(identity.Network, e.chainID),
		ChainID:    uint32(e.chainID),
		Dex:        util.NormalizeMarketDex(identity.Dex, e.exchange),
		AMMVersion: util.NormalizeMarketAMM(identity.AMMVersion, e.exchange),
	}
	count := atomic.AddUint64(&e.messageCount, 1)
	if count%500 == 0 {
		util.Infof("%s emitted %d market data messages", e.logPrefix, count)
	}
	if md.Dex == "" {
		md.Dex = util.NormalizeMarketDex(e.exchange, e.exchange)
	}
	if md.AMMVersion == "" {
		md.AMMVersion = util.DefaultAMMForDex(md.Dex)
	}
	util.Debugf("%s emit symbol=%s price=%.8f exchange=%s dex=%s amm=%s network=%s chain_id=%d", e.logPrefix, md.Symbol, md.Price, md.Exchange, md.Dex, md.AMMVersion, md.Network, e.chainID)
	e.out <- md
}

// --- Metadata & USD helpers ---
func (e *v3Engine) tryAcquireMetaSlot() bool {
	select {
	case e.metaSem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (e *v3Engine) releaseMetaSlot() {
	select {
	case <-e.metaSem:
	default:
	}
}

func (e *v3Engine) currentPricer() pricing.Pricer {
	return e.pricer
}

func (e *v3Engine) registerToken(meta v3TokenMeta) {
	if (meta.Address == common.Address{}) {
		return
	}
	if pricer := e.currentPricer(); pricer != nil {
		info := e.makeTokenInfo(meta)
		pricer.RegisterToken(info)
		if e.isStableSymbol(meta.Symbol) {
			pricer.RegisterStable(info)
		}
	}
}

func (e *v3Engine) makeTokenInfo(meta v3TokenMeta) pricing.TokenInfo {
	info := pricing.TokenInfo{
		Address:  meta.Address,
		Symbol:   meta.Symbol,
		Decimals: int(meta.Dec),
		DexAlias: e.exchange,
		Network:  e.network,
		ChainID:  e.chainID,
	}
	if e.isStableSymbol(meta.Symbol) {
		info.Bridge = strings.ToUpper(strings.TrimSpace(meta.Symbol))
	} else if meta.Address == e.wethAddress {
		info.Bridge = "WETH"
	}
	return info
}

func (e *v3Engine) updatePricing(pm *v3PoolMeta, price1Per0, price0Per1 *big.Rat, amount0, amount1 *big.Int) {
	if pm == nil || (!pm.Loaded && !pm.HasJSONMeta) {
		return
	}
	pricer := e.currentPricer()
	if pricer == nil {
		return
	}
	info0 := e.makeTokenInfo(pm.Token0)
	info1 := e.makeTokenInfo(pm.Token1)
	now := time.Now()
	weight := e.calcWeight(amount0, amount1, pm.Token0.Dec, pm.Token1.Dec)
	if val, ok := v3RatToFloat(price1Per0); ok {
		pricer.UpdatePair(info0, info1, val, weight, now)
	} else if val, ok := v3RatToFloat(price0Per1); ok {
		pricer.UpdatePair(info1, info0, val, weight, now)
	} else {
		return
	}
	identity := e.poolIdentity(pm)
	e.emitUSDWithPricer(pricer, pm, info0, identity, now)
	e.emitUSDWithPricer(pricer, pm, info1, identity, now)
}

func (e *v3Engine) emitUSDWithPricer(pricer pricing.Pricer, pm *v3PoolMeta, info pricing.TokenInfo, identity util.SymbolIdentity, ts time.Time) {
	if pricer == nil || e.out == nil {
		return
	}
	res, ok := pricer.ResolveUSD(info)
	if !ok || res.Price <= 0 || math.IsNaN(res.Price) || math.IsInf(res.Price, 0) {
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(info.Symbol))
	if symbol == "" {
		symbol = strings.ToUpper(strings.TrimPrefix(info.Address.Hex(), "0x"))
	}
	marketSymbol := symbol + "USD"
	if ts.IsZero() {
		ts = time.Now()
	}
	md := &pb.MarketData{
		Exchange:   e.exchange,
		Symbol:     marketSymbol,
		Price:      res.Price,
		Timestamp:  ts.UnixMilli(),
		Network:    util.NormalizeNetworkName(identity.Network, e.chainID),
		ChainID:    uint32(e.chainID),
		Dex:        util.NormalizeMarketDex(identity.Dex, e.exchange),
		AMMVersion: util.NormalizeMarketAMM(identity.AMMVersion, e.exchange),
	}
	if md.Dex == "" {
		md.Dex = util.NormalizeMarketDex(e.exchange, e.exchange)
	}
	if md.AMMVersion == "" {
		md.AMMVersion = util.DefaultAMMForDex(md.Dex)
	}
	e.out <- md
	util.Debugf("%s emit usd symbol=%s price=%.8f exchange=%s dex=%s amm=%s network=%s chain_id=%d", e.logPrefix, marketSymbol, res.Price, md.Exchange, md.Dex, md.AMMVersion, md.Network, e.chainID)
	if len(res.Route) > 0 && e.logAllEvents {
		util.Infof("%s usd %s price=%.8f weight=%.4f network=%s chain_id=%d route=%s", e.logPrefix, marketSymbol, res.Price, res.Weight, md.Network, md.ChainID, strings.Join(res.Route, "->"))
	}
}

func (e *v3Engine) poolIdentity(pm *v3PoolMeta) util.SymbolIdentity {
	if pm == nil {
		return util.ComposeIdentity("", "", e.network, e.chainID, e.exchange)
	}
	dex := pm.Descriptor.Dex
	if dex == "" {
		dex = pm.Dex
	}
	amm := pm.Descriptor.AMMVersion
	if amm == "" {
		amm = pm.AMMVersion
	}
	network := pm.Descriptor.Network
	if network == "" {
		network = pm.Network
	}
	return util.ComposeIdentity(dex, amm, network, e.chainID, e.exchange)
}

func v3RatToFloat(r *big.Rat) (float64, bool) {
	if r == nil {
		return 0, false
	}
	val, _ := new(big.Float).SetPrec(256).SetRat(r).Float64()
	if val <= 0 || math.IsNaN(val) || math.IsInf(val, 0) {
		return 0, false
	}
	return val, true
}

func (e *v3Engine) calcWeight(amount0, amount1 *big.Int, dec0, dec1 uint8) float64 {
	w0 := e.amountToFloat(amount0, dec0)
	w1 := e.amountToFloat(amount1, dec1)
	weight := math.Max(w0, w1)
	if weight <= 0 {
		return 1e-9
	}
	return weight
}

func (e *v3Engine) amountToFloat(amount *big.Int, dec uint8) float64 {
	if amount == nil {
		return 0
	}
	abs := new(big.Int).Abs(new(big.Int).Set(amount))
	if abs.Sign() == 0 {
		return 0
	}
	f := new(big.Float).SetPrec(256).SetInt(abs)
	if dec > 0 {
		den := new(big.Float).SetPrec(256).SetInt(e.pow10(dec))
		f.Quo(f, den)
	}
	val, _ := f.Float64()
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return 0
	}
	return val
}

func (e *v3Engine) ensurePoolMeta(pm *v3PoolMeta) error {
	if pm == nil {
		return fmt.Errorf("nil pool meta")
	}
	if pm.Loading {
		return nil
	}
	pm.Loading = true
	defer func() { pm.Loading = false }()
	t0, err := e.callAddress(pm.Addr, "0dfe1681")
	if err != nil {
		return err
	}
	t1, err := e.callAddress(pm.Addr, "d21220a7")
	if err != nil {
		return err
	}
	tm0, err := e.fetchTokenMeta(t0, pm.Token0)
	if err != nil {
		return err
	}
	tm1, err := e.fetchTokenMeta(t1, pm.Token1)
	if err != nil {
		return err
	}
	pm.Token0 = tm0
	pm.Token1 = tm1
	if tm0.Dec > 0 && tm1.Dec > 0 {
		pm.HasJSONMeta = true
	}
	e.registerToken(tm0)
	e.registerToken(tm1)
	return nil
}

func (e *v3Engine) callAddress(contract common.Address, selector string) (common.Address, error) {
	data := "0x" + selector
	resp, err := e.ethCall(contract, data)
	if err != nil {
		return common.Address{}, err
	}
	if len(resp) < 66 {
		return common.Address{}, fmt.Errorf("short address resp")
	}
	b, err := hexutil.Decode(resp)
	if err != nil {
		return common.Address{}, err
	}
	if len(b) < 32 {
		return common.Address{}, fmt.Errorf("addr bytes<32")
	}
	return common.BytesToAddress(b[12:32]), nil
}

func (e *v3Engine) fetchTokenMeta(addr common.Address, hint v3TokenMeta) (v3TokenMeta, error) {
	meta := v3TokenMeta{Address: addr, Symbol: strings.ToUpper(strings.TrimSpace(hint.Symbol)), Dec: hint.Dec}
	if hint.Address != addr {
		meta.Symbol = ""
		meta.Dec = 0
	}
	e.tokenMu.RLock()
	if cached, ok := e.tokenCache[addr]; ok {
		if meta.Symbol == "" {
			meta.Symbol = cached.Symbol
		}
		if meta.Dec == 0 {
			meta.Dec = cached.Dec
		}
	}
	e.tokenMu.RUnlock()
	if meta.Dec == 0 {
		if dec, err := e.callUint8(addr, "313ce567"); err == nil {
			meta.Dec = dec
		}
	}
	if meta.Symbol == "" {
		if sym, err := e.callSymbol(addr); err == nil {
			meta.Symbol = strings.ToUpper(strings.TrimSpace(sym))
		}
	}
	if meta.Symbol == "" {
		meta.Symbol = v3DefaultSymbol(addr)
	}
	e.tokenMu.Lock()
	e.tokenCache[addr] = meta
	e.tokenMu.Unlock()
	return meta, nil
}

func (e *v3Engine) callUint8(contract common.Address, selector string) (uint8, error) {
	data := "0x" + selector
	resp, err := e.ethCall(contract, data)
	if err != nil {
		return 0, err
	}
	b, err := hexutil.Decode(resp)
	if err != nil {
		return 0, err
	}
	if len(b) < 32 {
		return 0, fmt.Errorf("bad uint8 resp")
	}
	return uint8(b[31]), nil
}

func (e *v3Engine) callSymbol(contract common.Address) (string, error) {
	// Try standard symbol() -> selector 0x95d89b41 returning dynamic string
	resp, err := e.ethCall(contract, "0x95d89b41")
	if err == nil {
		// dynamic: offset (32) + length + data
		b, err2 := hexutil.Decode(resp)
		if err2 == nil && len(b) >= 96 {
			l := new(big.Int).SetBytes(b[32:64]).Uint64()
			if 64+int(l) <= len(b) {
				raw := b[64 : 64+int(l)]
				if isASCII(raw) {
					return strings.TrimSpace(string(raw)), nil
				}
			}
		}
	}
	// Fallback bytes32 (same selector) truncated or zero padded
	resp2, err2 := e.ethCall(contract, "0x95d89b41")
	if err2 != nil {
		return "", err2
	}
	b2, err3 := hexutil.Decode(resp2)
	if err3 != nil {
		return "", err3
	}
	if len(b2) < 32 {
		return "", fmt.Errorf("bytes32 short")
	}
	trimmed := bytes.TrimRight(b2[:32], "\x00")
	if len(trimmed) == 0 {
		return "", nil
	}
	if isASCII(trimmed) {
		return string(trimmed), nil
	}
	return hexutil.Encode(trimmed), nil
}

func isASCII(b []byte) bool {
	for _, c := range b {
		if c < 32 || c > 126 {
			return false
		}
	}
	return true
}

func (e *v3Engine) ethCall(to common.Address, data string) (string, error) {
	if e.httpURL == "" {
		return "", fmt.Errorf("http url empty")
	}
	reqBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"%s","data":"%s"},"latest"]}`, to.Hex(), data)
	r, err := http.NewRequest("POST", e.httpURL, strings.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := e.httpClient.Do(r)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("eth_call status=%d %s", resp.StatusCode, string(body))
	}
	var parsed struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("eth_call err %s", parsed.Error.Message)
	}
	return parsed.Result, nil
}

func (e *v3Engine) decimalAdjust(dec0, dec1 uint8) *big.Rat {
	num := e.pow10(dec0)
	den := e.pow10(dec1)
	return new(big.Rat).SetFrac(num, den)
}

func (e *v3Engine) pow10(dec uint8) *big.Int {
	e.pow10Mu.RLock()
	if v, ok := e.pow10Cache[dec]; ok {
		e.pow10Mu.RUnlock()
		return v
	}
	e.pow10Mu.RUnlock()
	v := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil)
	e.pow10Mu.Lock()
	e.pow10Cache[dec] = v
	e.pow10Mu.Unlock()
	return v
}

func (e *v3Engine) deriveUSD(pm *v3PoolMeta, price1Per0, price0Per1 *big.Rat) string {
	if !pm.Loaded && !pm.HasJSONMeta {
		return ""
	}
	sym0 := strings.ToUpper(pm.Token0.Symbol)
	sym1 := strings.ToUpper(pm.Token1.Symbol)
	usdParts := []string{}
	// 1. Обновление WETHUSD (WETH-стэйбл пул)
	updated := false
	if (pm.Token0.Address == e.wethAddress && e.isStableSymbol(sym1)) && price0Per1 != nil {
		stablePerWeth := v3Invert(price0Per1)
		e.maybeSetWETHUSD(stablePerWeth, sym1, pm.PairName)
		updated = true
	} else if (pm.Token1.Address == e.wethAddress && e.isStableSymbol(sym0)) && price1Per0 != nil {
		stablePerWeth := v3Invert(price1Per0)
		e.maybeSetWETHUSD(stablePerWeth, sym0, pm.PairName)
		updated = true
	}

	e.wethMu.RLock()
	wethUSD := e.wethUSD
	wethStable := e.wethStable
	e.wethMu.RUnlock()

	// Helper для добавления BA (базовый актив) позже
	type baInfo struct {
		sym    string
		usd    *big.Rat
		stable string
	}
	var ba *baInfo

	// 2. Стэйбл <-> токен (прямой USD) — цена токена в USD напрямую
	if e.isStableSymbol(sym0) && !e.isStableSymbol(sym1) && price1Per0 != nil {
		usd := v3Invert(price1Per0)
		if usd != nil {
			usdParts = append(usdParts, fmt.Sprintf("%sUSD=%s %s", sym1, v3Format(usd, 6), sym0))
			ba = &baInfo{sym: sym1, usd: usd, stable: sym0}
		}
	} else if e.isStableSymbol(sym1) && !e.isStableSymbol(sym0) && price0Per1 != nil {
		usd := v3Invert(price0Per1)
		if usd != nil {
			usdParts = append(usdParts, fmt.Sprintf("%sUSD=%s %s", sym0, v3Format(usd, 6), sym1))
			ba = &baInfo{sym: sym0, usd: usd, stable: sym1}
		}
	}

	// 3. Токен-WETH — деривация через WETHUSD
	if wethUSD != nil {
		if pm.Token0.Address == e.wethAddress && !e.isStableSymbol(sym1) && price1Per0 != nil {
			token1PerWeth := price1Per0
			usd := new(big.Rat).Mul(v3Invert(token1PerWeth), wethUSD)
			usdParts = appendIfMissing(usdParts, fmt.Sprintf("%sUSD=%s %s", sym1, v3Format(usd, 6), wethStable))
			if ba == nil {
				ba = &baInfo{sym: sym1, usd: usd, stable: wethStable}
			}
		} else if pm.Token1.Address == e.wethAddress && !e.isStableSymbol(sym0) && price0Per1 != nil {
			token0PerWeth := price0Per1
			usd := new(big.Rat).Mul(v3Invert(token0PerWeth), wethUSD)
			usdParts = appendIfMissing(usdParts, fmt.Sprintf("%sUSD=%s %s", sym0, v3Format(usd, 6), wethStable))
			if ba == nil {
				ba = &baInfo{sym: sym0, usd: usd, stable: wethStable}
			}
		}
	}

	// 4. Стэйбл-стэйбл пул — можно указать BA=первый стэйбл (цена 1)
	if e.isStableSymbol(sym0) && e.isStableSymbol(sym1) && ba == nil {
		one := big.NewRat(1, 1)
		ba = &baInfo{sym: sym0, usd: one, stable: sym0}
		// Не добавляем дублирующий XXXUSD=1 если уже есть другие
		usdParts = appendIfMissing(usdParts, fmt.Sprintf("%sUSD=1.000000 %s", sym0, sym0))
	}

	// 5. Добавить WETHUSD в вывод если только что обновили
	if updated && wethUSD != nil {
		usdParts = append([]string{fmt.Sprintf("WETHUSD=%s %s", v3Format(wethUSD, 6), wethStable)}, usdParts...)
	}

	// 6. Добавить BAUSD
	if ba != nil && ba.usd != nil {
		usdParts = append(usdParts, fmt.Sprintf("BA=%s BAUSD=%s %s", ba.sym, v3Format(ba.usd, 6), ba.stable))
	}

	if len(usdParts) == 0 {
		return ""
	}
	if e.logAllEvents {
		util.Infof("%s usd route %s", e.logPrefix, strings.Join(usdParts, " "))
	}
	return strings.Join(usdParts, " ")
}

func appendIfMissing(sl []string, val string) []string {
	for _, v := range sl {
		if v == val {
			return sl
		}
	}
	return append(sl, val)
}

func (e *v3Engine) maybeSetWETHUSD(stablePerWeth *big.Rat, stableSym, src string) {
	if stablePerWeth == nil || stablePerWeth.Sign() <= 0 {
		return
	}
	stableSym = strings.ToUpper(strings.TrimSpace(stableSym))
	if !e.isStableSymbol(stableSym) {
		util.Debugf("%s skip wethusd update unknown stable=%s", e.logPrefix, stableSym)
		return
	}
	// WETHUSD = stable per WETH
	if !v3SanityWETH(stablePerWeth) {
		return
	}
	e.wethMu.Lock()
	defer e.wethMu.Unlock()
	if e.wethUSD != nil {
		// Only upgrade if higher priority
		if v3Priority(stableSym) <= v3Priority(e.wethStable) {
			return
		}
	}
	e.wethUSD = new(big.Rat).Set(stablePerWeth)
	e.wethStable = stableSym
	util.Infof("%s wethusd=%s stable=%s src=%s", e.logPrefix, v3Format(e.wethUSD, 6), stableSym, src)
}

func v3Priority(s string) int {
	switch s {
	case "USDC":
		return 3
	case "USDT":
		return 2
	case "DAI":
		return 1
	default:
		return 0
	}
}
func v3SanityWETH(r *big.Rat) bool {
	f, _ := new(big.Float).SetRat(r).Float64()
	if f < 300 || f > 100000 {
		return false
	}
	return true
}

// --- Utils ---
func v3Mask(u string) string {
	i := strings.LastIndex(u, "/")
	if i == -1 {
		return u
	}
	tail := u[i+1:]
	if len(tail) <= 6 {
		return u[:i+1] + "***"
	}
	return u[:i+1] + tail[:3] + "***" + tail[len(tail)-2:]
}

// Простая загрузка .env (копия упрощённая)
func v3LoadDotEnv(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		util.Debugf("uniswap_v3 .env not found (%s)", path)
		return
	}
	for idx, ln := range bytes.Split(b, []byte("\n")) {
		s := strings.TrimSpace(string(ln))
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		eq := strings.Index(s, "=")
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(s[:eq])
		v := strings.TrimSpace(s[eq+1:])
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		if _, exists := os.LookupEnv(k); exists {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			util.Errorf("uniswap_v3 .env set env line=%d key=%s err=%v", idx+1, k, err)
			continue
		}
		disp := v
		up := strings.ToUpper(k)
		if strings.Contains(up, "KEY") || strings.Contains(up, "SECRET") || strings.Contains(up, "TOKEN") || len(v) > 10 {
			if len(v) > 6 {
				disp = v[:3] + "***" + v[len(v)-2:]
			} else {
				disp = "***"
			}
		}
		util.Infof("uniswap_v3 .env %s=%s", k, disp)
	}
}
