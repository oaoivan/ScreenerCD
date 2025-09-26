package main

// Подписка на ВСЕ Uniswap V3 пулы (ETH сеть) из geckoterminal_pools.json одним WS подключением Alchemy.
// Цель: стрим всех Swap/Sync-подобных событий (в V3 – Swap) для проверки формулы цены.
// Минимальная версия: логирует sqrtPriceX96 и выводит price(token1/token0) и inverse без нормализации по decimals (если ещё не загружены).
// При первом событии пула — лениво подтягиваем token0/token1 metadata (symbol, decimals) через eth_call по тому же WS.
// Ограничений по времени / количеству событий нет — останавливать Ctrl+C.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/gorilla/websocket"
)

// --- Константы ---
const (
	v3DefaultMainnetTemplate = "wss://eth-mainnet.g.alchemy.com/v2/%s"
	v3GeckoDefaultPath       = "Temp/geckoterminal_pools.json"
	v3BatchSizeDefault       = 150
	v3PingInterval           = 25 * time.Second
	v3ReconnectBase          = 2 * time.Second
	v3ReconnectMax           = 30 * time.Second
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
	Addr     common.Address
	PairName string
	Token0   v3TokenMeta
	Token1   v3TokenMeta
	Loaded   bool // метаданные токенов загружены
	Loading  bool
	LoadErr  error
}

// Глобальные
var (
	v3Pools          = make(map[common.Address]*v3PoolMeta)
	v3BatchSize      int
	v3ABI            abi.ABI
	v3EventBySig     = make(map[common.Hash]abi.Event)
	v3TwoPow192      = new(big.Int).Lsh(big.NewInt(1), 192)
	v3WSURL          string
	v3StopOnErrAck   bool
	v3LogAllEvents   bool
	v3DecodeSwapOnly bool
	v3HTTPURL        string
	v3HTTPOnce       sync.Once
	v3HTTPClient     = &http.Client{Timeout: 10 * time.Second}
	v3TokenCache     = make(map[common.Address]v3TokenMeta)
	v3TokenMu        sync.RWMutex
	v3Pow10Cache     = make(map[uint8]*big.Int)
	v3Pow10Mu        sync.RWMutex
	v3WETHAddress    = common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")
	v3StableSyms     = map[string]bool{"USDC": true, "USDT": true, "DAI": true, "TUSD": true, "FDUSD": true}
	v3WETHUSD        *big.Rat
	v3WETHUSDStable  string
	v3WethMu         sync.RWMutex
	v3MetaSem        = make(chan struct{}, 8) // ограничение параллельных metadata fetch
)

// Event сигнатуры V3 (минимум Swap). Keccak("Swap(address,address,int256,int256,uint160,uint128,int24)")
var v3SwapSig = common.HexToHash("0xc42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67")

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	flag.IntVar(&v3BatchSize, "batch", v3BatchSizeDefault, "размер батча адресов в одном eth_subscribe")
	flag.BoolVar(&v3StopOnErrAck, "stopOnAckErr", false, "остановить при ошибке подписки")
	flag.BoolVar(&v3LogAllEvents, "logAll", false, "логировать все события (иначе только Swap)")
	flag.BoolVar(&v3DecodeSwapOnly, "swapOnly", true, "пытаться декодировать только Swap (быстрее)")
	flag.Parse()

	v3LoadDotEnv(".env")
	if err := v3InitABI(); err != nil {
		log.Fatalf("init abi: %v", err)
	}
	if err := v3ResolveWS(); err != nil {
		log.Fatalf("resolve ws: %v", err)
	}
	if err := v3LoadPools(); err != nil {
		log.Fatalf("load pools: %v", err)
	}
	if len(v3Pools) == 0 {
		log.Fatalf("no v3 pools loaded")
	}

	log.Printf("[INFO] total V3 pools=%d (will subscribe)", len(v3Pools))
	v3RunLoop()
}

// --- Init / Load ---
func v3InitABI() error {
	b, err := os.ReadFile("ABI/Uniswap/V3/UniswapV3Pool.json")
	if err != nil {
		return err
	}
	parsed, err := abi.JSON(bytes.NewReader(b))
	if err != nil {
		return err
	}
	v3ABI = parsed
	for _, ev := range v3ABI.Events {
		v3EventBySig[ev.ID] = ev
	}
	return nil
}

func v3ResolveWS() error {
	if direct := strings.TrimSpace(os.Getenv("ALCHEMY_WS_URL")); direct != "" {
		v3WSURL = direct
		return nil
	}
	key := strings.TrimSpace(os.Getenv("ALCHEMY_API_KEY"))
	if key == "" {
		return fmt.Errorf("set ALCHEMY_WS_URL or ALCHEMY_API_KEY")
	}
	v3WSURL = fmt.Sprintf(v3DefaultMainnetTemplate, key)
	v3HTTPOnce.Do(func() {
		if h := strings.TrimSpace(os.Getenv("ALCHEMY_HTTP_URL")); h != "" {
			v3HTTPURL = h
			return
		}
		v3HTTPURL = fmt.Sprintf("https://eth-mainnet.g.alchemy.com/v2/%s", key)
	})
	return nil
}

func v3LoadPools() error {
	path := strings.TrimSpace(os.Getenv("GECKO_POOLS_JSON"))
	if path == "" {
		path = v3GeckoDefaultPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var f v3GeckoFile
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	added := 0
	for _, e := range f.Entries {
		if !strings.EqualFold(e.Dex, "uniswap_v3") {
			continue
		}
		if !strings.Contains(strings.ToLower(e.Network), "eth") {
			continue
		}
		addrHex := e.PoolID
		if addrHex == "" {
			addrHex = e.PoolAddress
		}
		if !common.IsHexAddress(addrHex) {
			continue
		}
		addr := common.HexToAddress(addrHex)
		if _, exists := v3Pools[addr]; exists {
			continue
		}
		v3Pools[addr] = &v3PoolMeta{Addr: addr, PairName: e.PairName}
		added++
		if added <= 20 {
			log.Printf("[INFO] add V3 pool %s addr=%s", e.PairName, addr.Hex())
		}
	}
	log.Printf("[INFO] loaded V3 pools=%d", added)
	return nil
}

// --- Run Loop ---
func v3RunLoop() {
	backoff := v3ReconnectBase
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	for {
		select {
		case <-sigCh:
			log.Printf("[INFO] interrupt -> exit")
			return
		default:
		}
		conn, err := v3Dial()
		if err != nil {
			log.Printf("[ERR ] dial: %v", err)
			backoff = v3NextBackoff(backoff)
			time.Sleep(backoff)
			continue
		}
		log.Printf("[INFO] WS connected %s", v3Mask(v3WSURL))
		if err := v3SubscribeAll(conn); err != nil {
			log.Printf("[ERR ] subscribe: %v", err)
			conn.Close()
			backoff = v3NextBackoff(backoff)
			time.Sleep(backoff)
			continue
		}
		backoff = v3ReconnectBase
		ctx, cancel := context.WithCancel(context.Background())
		msgs := make(chan []byte, 4096)
		errs := make(chan error, 1)
		go v3Ping(ctx, conn)
		go v3Read(conn, msgs, errs)
		run := true
		for run {
			select {
			case raw, ok := <-msgs:
				if !ok {
					log.Printf("[INFO] reader closed -> reconnect")
					run = false
					continue
				}
				v3Handle(raw)
			case e := <-errs:
				if e != nil {
					log.Printf("[WARN] ws error: %v", e)
					run = false
				}
			case <-sigCh:
				log.Printf("[INFO] interrupt signal")
				cancel()
				conn.Close()
				return
			}
		}
		cancel()
		conn.Close()
		log.Printf("[INFO] reconnect in %s", backoff)
		time.Sleep(backoff)
		backoff = v3NextBackoff(backoff)
	}
}

func v3Dial() (*websocket.Conn, error) {
	d := *websocket.DefaultDialer
	d.EnableCompression = true
	c, _, err := d.Dial(v3WSURL, nil)
	return c, err
}

func v3SubscribeAll(conn *websocket.Conn) error {
	addresses := make([]string, 0, len(v3Pools))
	for a := range v3Pools {
		addresses = append(addresses, a.Hex())
	}
	sort.Strings(addresses)
	batches := 0
	id := 1
	for start := 0; start < len(addresses); start += v3BatchSize {
		end := start + v3BatchSize
		if end > len(addresses) {
			end = len(addresses)
		}
		part := addresses[start:end]
		req := v3RPCRequest{JSONRPC: "2.0", ID: id, Method: "eth_subscribe", Params: []interface{}{"logs", map[string]interface{}{"address": part}}}
		if err := conn.WriteJSON(req); err != nil {
			return fmt.Errorf("batch %d: %w", batches, err)
		}
		log.Printf("[INFO] subscribe batch=%d id=%d size=%d", batches, id, len(part))
		batches++
		id++
	}
	log.Printf("[INFO] total subscribe batches=%d", batches)
	return nil
}

// --- WS helpers ---
func v3Ping(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(v3PingInterval)
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
func v3Read(c *websocket.Conn, out chan<- []byte, errs chan<- error) {
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
func v3Handle(raw []byte) {
	if ack, ok := v3TryAck(raw); ok {
		if ack.Error != nil {
			log.Printf("[ERR ] ack %d %s", ack.Error.Code, ack.Error.Message)
			if v3StopOnErrAck {
				log.Fatalf("ack error stop")
			}
		} else {
			log.Printf("[INFO] subscribed id=%s", ack.Result)
		}
		return
	}
	note, ok := v3TryNote(raw)
	if !ok {
		return
	}
	v3Process(note.Params.Result)
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
func v3Process(l v3LogItem) {
	if l.Removed || len(l.Topics) == 0 {
		return
	}
	sig := common.HexToHash(l.Topics[0])
	if v3DecodeSwapOnly && sig != v3SwapSig {
		return
	}
	addr := common.HexToAddress(l.Address)
	pm, ok := v3Pools[addr]
	if !ok {
		return
	}
	if sig == v3SwapSig {
		v3HandleSwap(pm, l)
	} else if v3LogAllEvents {
		log.Printf("[EVT ] addr=%s topic=%s tx=%s", addr.Hex(), sig.Hex(), l.TransactionHash)
	}
}

func v3HandleSwap(pm *v3PoolMeta, l v3LogItem) {
	// Unpack non-indexed data для Swap: amount0, amount1, sqrtPriceX96, liquidity, tick
	if len(l.Data) < 2 {
		return
	}
	dataBytes, err := hexutil.Decode(l.Data)
	if err != nil {
		return
	}
	// По ABI порядок: int256 amount0, int256 amount1, uint160 sqrtPriceX96, uint128 liquidity, int24 tick
	// ABI-пакетирование: каждая 32-байтовая ячейка.
	if len(dataBytes) < 32*5 {
		return
	}
	amount0 := v3DecodeInt256(dataBytes[0:32])
	amount1 := v3DecodeInt256(dataBytes[32:64])
	sqrtPriceX96 := new(big.Int).SetBytes(dataBytes[64:96]) // uint160 в 32 байтах
	// liquidity := new(big.Int).SetBytes(dataBytes[96:128]) // можно логировать при желании
	// tick := v3DecodeInt24(dataBytes[128:160]) // последние 32 байта содержат int24 в хвосте
	// Цена raw = (sqrtP^2)/2^192 = token1/token0
	rawPrice := v3PriceFromSqrt(sqrtPriceX96) // token1/token0 без decimal adjust (Raw)
	if rawPrice == nil {
		return
	}
	// Ensure metadata (token addresses, symbols, decimals) — лениво.
	if !pm.Loaded {
		if v3TryAcquireMetaSlot() { // не блокируем если лимит
			if err := v3EnsurePoolMeta(pm); err != nil {
				pm.LoadErr = err
				pm.Loaded = false
				pm.Loading = false
				log.Printf("[WARN] meta pool=%s err=%v", pm.Addr.Hex(), err)
			} else {
				pm.Loaded = true
			}
			v3ReleaseMetaSlot()
		}
	}

	var price1Per0, price0Per1 *big.Rat
	var priceNote string
	if pm.Loaded && pm.Token0.Dec > 0 && pm.Token1.Dec > 0 { // нормализуем
		adj := v3DecimalAdjust(pm.Token0.Dec, pm.Token1.Dec)
		price1Per0 = new(big.Rat).Mul(rawPrice, adj) // 1 token0 -> token1
		price0Per1 = v3Invert(price1Per0)
		priceNote = "norm"
	} else {
		price1Per0 = rawPrice
		price0Per1 = v3Invert(rawPrice)
		priceNote = "raw"
	}

	// USD derivation (опционально, быстрый путь)
	usdLines := v3DeriveUSD(pm, price1Per0, price0Per1)

	log.Printf("[SWAP] pool=%s addr=%s sqrtP=%s mode=%s p1per0=%s p0per1=%s amt0=%s amt1=%s blk=%s tx=%s %s", pm.PairName, pm.Addr.Hex(), sqrtPriceX96.String(), priceNote, v3Format(price1Per0, 8), v3Format(price0Per1, 8), amount0.String(), amount1.String(), l.BlockNumber, l.TransactionHash, usdLines)
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
func v3PriceFromSqrt(s *big.Int) *big.Rat {
	if s == nil || s.Sign() == 0 {
		return nil
	}
	sq := new(big.Int).Mul(s, s)
	return new(big.Rat).SetFrac(sq, v3TwoPow192)
}
func v3Format(r *big.Rat, prec int) string {
	if r == nil {
		return "?"
	}
	f := new(big.Float).SetPrec(256).SetRat(r)
	return f.Text('f', prec)
}

// --- Metadata & USD helpers ---
func v3TryAcquireMetaSlot() bool {
	select {
	case v3MetaSem <- struct{}{}:
		return true
	default:
		return false
	}
}
func v3ReleaseMetaSlot() {
	select {
	case <-v3MetaSem:
	default:
	}
}

func v3EnsurePoolMeta(pm *v3PoolMeta) error {
	if pm == nil {
		return fmt.Errorf("nil pool meta")
	}
	if pm.Loaded || pm.Loading {
		return nil
	}
	pm.Loading = true
	defer func() { pm.Loading = false }()
	// Fetch token0/token1 via eth_call
	t0, err := v3CallAddress(pm.Addr, "0dfe1681") // token0()
	if err != nil {
		return err
	}
	t1, err := v3CallAddress(pm.Addr, "d21220a7") // token1()
	if err != nil {
		return err
	}
	tm0, err := v3FetchTokenMeta(t0)
	if err != nil {
		return err
	}
	tm1, err := v3FetchTokenMeta(t1)
	if err != nil {
		return err
	}
	pm.Token0 = tm0
	pm.Token1 = tm1
	return nil
}

// v3CallAddress calls a contract method returning address (first 32 bytes right-padded) using function selector hex (8 chars w/o 0x)
func v3CallAddress(contract common.Address, selector string) (common.Address, error) {
	data := "0x" + selector
	resp, err := v3EthCall(contract, data)
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

func v3FetchTokenMeta(addr common.Address) (v3TokenMeta, error) {
	v3TokenMu.RLock()
	if m, ok := v3TokenCache[addr]; ok {
		v3TokenMu.RUnlock()
		return m, nil
	}
	v3TokenMu.RUnlock()
	dec, _ := v3CallUint8(addr, "313ce567") // decimals()
	sym, _ := v3CallSymbol(addr)
	tm := v3TokenMeta{Address: addr, Symbol: sym, Dec: dec}
	v3TokenMu.Lock()
	v3TokenCache[addr] = tm
	v3TokenMu.Unlock()
	return tm, nil
}

func v3CallUint8(contract common.Address, selector string) (uint8, error) {
	data := "0x" + selector
	resp, err := v3EthCall(contract, data)
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

func v3CallSymbol(contract common.Address) (string, error) {
	// Try standard symbol() -> selector 0x95d89b41 returning dynamic string
	resp, err := v3EthCall(contract, "0x95d89b41")
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
	resp2, err2 := v3EthCall(contract, "0x95d89b41")
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

// v3EthCall performs a minimal eth_call via HTTP
func v3EthCall(to common.Address, data string) (string, error) {
	if v3HTTPURL == "" {
		return "", fmt.Errorf("http url empty")
	}
	reqBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"%s","data":"%s"},"latest"]}`, to.Hex(), data)
	r, err := http.NewRequest("POST", v3HTTPURL, strings.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := v3HTTPClient.Do(r)
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

func v3DecimalAdjust(dec0, dec1 uint8) *big.Rat {
	num := v3Pow10(dec0)
	den := v3Pow10(dec1)
	return new(big.Rat).SetFrac(num, den)
}
func v3Pow10(dec uint8) *big.Int {
	v3Pow10Mu.RLock()
	if v, ok := v3Pow10Cache[dec]; ok {
		v3Pow10Mu.RUnlock()
		return v
	}
	v3Pow10Mu.RUnlock()
	v := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil)
	v3Pow10Mu.Lock()
	v3Pow10Cache[dec] = v
	v3Pow10Mu.Unlock()
	return v
}

func v3DeriveUSD(pm *v3PoolMeta, price1Per0, price0Per1 *big.Rat) string {
	if !pm.Loaded {
		return ""
	}
	sym0 := strings.ToUpper(pm.Token0.Symbol)
	sym1 := strings.ToUpper(pm.Token1.Symbol)
	usdParts := []string{}
	// 1. Обновление WETHUSD (WETH-стэйбл пул)
	updated := false
	if (pm.Token0.Address == v3WETHAddress && v3StableSyms[sym1]) && price0Per1 != nil { // WETH - STABLE (token0=WETH, token1=STABLE)
		stablePerWeth := v3Invert(price0Per1)
		v3MaybeSetWETHUSD(stablePerWeth, sym1, pm.PairName)
		updated = true
	} else if (pm.Token1.Address == v3WETHAddress && v3StableSyms[sym0]) && price1Per0 != nil { // STABLE - WETH (token1=WETH)
		stablePerWeth := v3Invert(price1Per0)
		v3MaybeSetWETHUSD(stablePerWeth, sym0, pm.PairName)
		updated = true
	}

	v3WethMu.RLock()
	wethUSD := v3WETHUSD
	wethStable := v3WETHUSDStable
	v3WethMu.RUnlock()

	// Helper для добавления BA (базовый актив) позже
	type baInfo struct {
		sym    string
		usd    *big.Rat
		stable string
	}
	var ba *baInfo

	// 2. Стэйбл <-> токен (прямой USD) — цена токена в USD напрямую
	if v3StableSyms[sym0] && !v3StableSyms[sym1] && price1Per0 != nil { // token0=STABLE, token1=TOKEN : price1Per0 = token1 per token0 => 1 token0 (USD) = price1Per0 token1 => 1 token1 = 1/price1Per0 USD
		usd := v3Invert(price1Per0)
		if usd != nil {
			usdParts = append(usdParts, fmt.Sprintf("%sUSD=%s %s", sym1, v3Format(usd, 6), sym0))
			ba = &baInfo{sym: sym1, usd: usd, stable: sym0}
		}
	} else if v3StableSyms[sym1] && !v3StableSyms[sym0] && price0Per1 != nil { // token1=STABLE, token0=TOKEN : price0Per1 = token0 per token1 => 1 token1(USD)=price0Per1 token0 => 1 token0 = 1/price0Per1 USD
		usd := v3Invert(price0Per1)
		if usd != nil {
			usdParts = append(usdParts, fmt.Sprintf("%sUSD=%s %s", sym0, v3Format(usd, 6), sym1))
			ba = &baInfo{sym: sym0, usd: usd, stable: sym1}
		}
	}

	// 3. Токен-WETH — деривация через WETHUSD
	if wethUSD != nil {
		if pm.Token0.Address == v3WETHAddress && !v3StableSyms[sym1] && price1Per0 != nil { // WETH -> token1
			token1PerWeth := price1Per0
			usd := new(big.Rat).Mul(v3Invert(token1PerWeth), wethUSD)
			usdParts = appendIfMissing(usdParts, fmt.Sprintf("%sUSD=%s %s", sym1, v3Format(usd, 6), wethStable))
			if ba == nil {
				ba = &baInfo{sym: sym1, usd: usd, stable: wethStable}
			}
		} else if pm.Token1.Address == v3WETHAddress && !v3StableSyms[sym0] && price0Per1 != nil { // token0 -> WETH
			token0PerWeth := price0Per1
			usd := new(big.Rat).Mul(v3Invert(token0PerWeth), wethUSD)
			usdParts = appendIfMissing(usdParts, fmt.Sprintf("%sUSD=%s %s", sym0, v3Format(usd, 6), wethStable))
			if ba == nil {
				ba = &baInfo{sym: sym0, usd: usd, stable: wethStable}
			}
		}
	}

	// 4. Стэйбл-стэйбл пул — можно указать BA=первый стэйбл (цена 1)
	if v3StableSyms[sym0] && v3StableSyms[sym1] && ba == nil {
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

func v3MaybeSetWETHUSD(stablePerWeth *big.Rat, stableSym, src string) {
	if stablePerWeth == nil || stablePerWeth.Sign() <= 0 {
		return
	}
	// WETHUSD = stable per WETH
	if !v3SanityWETH(stablePerWeth) {
		return
	}
	v3WethMu.Lock()
	defer v3WethMu.Unlock()
	if v3WETHUSD != nil {
		// Only upgrade if higher priority
		if v3Priority(stableSym) <= v3Priority(v3WETHUSDStable) {
			return
		}
	}
	v3WETHUSD = new(big.Rat).Set(stablePerWeth)
	v3WETHUSDStable = stableSym
	log.Printf("[USD ] WETHUSD=%s %s src=%s", v3Format(v3WETHUSD, 6), stableSym, src)
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
		log.Printf("[INFO] .env not found (%s)", path)
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
			log.Printf("[ERR ] set env line=%d key=%s err=%v", idx+1, k, err)
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
		log.Printf("[INFO] .env %s=%s", k, disp)
	}
}
