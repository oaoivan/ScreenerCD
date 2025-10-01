package main

// Быстрый прототип подписки на Uniswap V2 пулы через Alchemy WS.
// Цель: получить цены минимум двух базовых активов (не стейбл) из резервов (Sync события) и выйти.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gorilla/websocket"
)

// --- Константы ---
const (
	v2DefaultMainnetTemplate = "wss://eth-mainnet.g.alchemy.com/v2/%s"
	v2Timeout                = 8 * time.Minute
	v2GeckoDefaultPath       = "Temp/geckoterminal_pools.json"
	v2BatchSize              = 150 // адресов на одну подписку (безопасный лимит)
	v2WETHAddress            = "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"
	v2DefaultHTTPTemplate    = "https://eth-mainnet.g.alchemy.com/v2/%s"
)

// --- Структуры входного JSON ---
type v2IntOrString int

func (v *v2IntOrString) UnmarshalJSON(b []byte) error {
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
		*v = v2IntOrString(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(bb, &n); err != nil {
		return err
	}
	*v = v2IntOrString(n)
	return nil
}

type v2GeckoPool struct {
	Dex         string `json:"dex"`
	PairName    string `json:"pair_name"`
	PoolID      string `json:"pool_id"` // для V2 фактически адрес пары
	PoolAddress string `json:"pool_address"`
	Network     string `json:"network"`
	Token0      struct {
		Address  string        `json:"address"`
		Symbol   string        `json:"symbol"`
		Decimals v2IntOrString `json:"decimals"`
	} `json:"token0"`
	Token1 struct {
		Address  string        `json:"address"`
		Symbol   string        `json:"symbol"`
		Decimals v2IntOrString `json:"decimals"`
	} `json:"token1"`
}

type v2GeckoFile struct {
	Entries []v2GeckoPool `json:"entries"`
}

// --- Мета по пулу ---
type v2PoolMeta struct {
	Addr      common.Address
	PairName  string
	Symbol0   string
	Symbol1   string
	Address0  common.Address
	Address1  common.Address
	Decimals0 int
	Decimals1 int
	BaseIs0   bool // какой токен считаем базовым (цена 1 BASE в QUOTE)
	LastPrice *big.Rat
	GotFirst  bool
	HasStable bool
	HasWETH   bool
	StableSym string
}

// --- RPC структуры ---
type v2RPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type v2SubAck struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  string `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type v2SubNote struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		Subscription string    `json:"subscription"`
		Result       v2LogItem `json:"result"`
	} `json:"params"`
}

type v2LogItem struct {
	Address         string   `json:"address"`
	Data            string   `json:"data"`
	Topics          []string `json:"topics"`
	BlockNumber     string   `json:"blockNumber"`
	TransactionHash string   `json:"transactionHash"`
	Removed         bool     `json:"removed"`
}

// --- Глобальные ---
var (
	v2Pools        = make(map[common.Address]*v2PoolMeta)
	v2NeedDistinct int
	v2Once         bool
	v2Stable       = map[string]bool{"USDC": true, "USDT": true, "DAI": true, "TUSD": true, "FDUSD": true, "USDD": true}
	v2StableAddr   = map[string]string{ // lowercase address -> symbol
		strings.ToLower("0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"): "USDC",
		strings.ToLower("0xdac17f958d2ee523a2206206994597c13d831ec7"): "USDT",
		strings.ToLower("0x6b175474e89094c44da98b954eedeac495271d0f"): "DAI",
		strings.ToLower("0x0000000000085d4780B73119b644AE5ecd22b376"): "TUSD",
	}
	v2SyncSig         = common.HexToHash("0x1c411e9a96e071241c2f21f7726b17ae89e3cab4c78be50e062b03a9fffbbad1") // keccak256("Sync(uint112,uint112)")
	v2WETHUSD         *big.Rat
	v2WETHUSDStable   string
	v2EventLimit      int // если >0, выходим после указанного числа принятых (после фильтра) событий цены
	v2EventsProcessed int
	// Hard reference pools (real Uniswap V2) with actual token0/token1 ordering
	// USDC/WETH   0xB4e16d0168e52d35CaCD2c6185b44281Ec28C9Dc  (token0=USDC, token1=WETH)
	// WETH/USDT   0x0d4a11d5EEaaC28EC3F61d100daf4d40471f1852  (token0=WETH, token1=USDT)
	// DAI/WETH    0xA478c2975Ab1Ea89e8196811F51A7B7Ade33eB11  (token0=DAI,  token1=WETH)
	v2HardWethStable = []struct{ Addr, Stable string }{
		{Addr: "0xB4e16d0168e52d35CaCD2c6185b44281Ec28C9Dc", Stable: "USDC"},
		{Addr: "0x0d4a11d5EEaaC28EC3F61d100daf4d40471f1852", Stable: "USDT"},
		{Addr: "0xA478c2975Ab1Ea89e8196811F51A7B7Ade33eB11", Stable: "DAI"},
	}
	// Каноническая мета по известным токенам (lower address -> symbol, decimals, class)
	v2Canonical = map[string]struct {
		Sym    string
		Dec    int
		Stable bool
		WETH   bool
	}{
		strings.ToLower(v2WETHAddress):                                {Sym: "WETH", Dec: 18, WETH: true},
		strings.ToLower("0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"): {Sym: "USDC", Dec: 6, Stable: true},
		strings.ToLower("0xdac17f958d2ee523a2206206994597c13d831ec7"): {Sym: "USDT", Dec: 6, Stable: true},
		strings.ToLower("0x6b175474e89094c44da98b954eedeac495271d0f"): {Sym: "DAI", Dec: 18, Stable: true},
		strings.ToLower("0xc011a73ee8576fb46f5e1c5751ca3b9fe0af2a6f"): {Sym: "SNX", Dec: 18},
		strings.ToLower("0x514910771af9ca656af840dff83e8264ecf986ca"): {Sym: "LINK", Dec: 18},
		strings.ToLower("0x1f9840a85d5af5bf1d1762f925bdaddc4201f984"): {Sym: "UNI", Dec: 18},
		strings.ToLower("0x45804880De22913dAFE09f4980848ECE6EcbAf78"): {Sym: "PAXG", Dec: 18},
		strings.ToLower("0x6982508145454Ce325dDbE47a25d4ec3d2311933"): {Sym: "PEPE", Dec: 18},
		strings.ToLower("0xe53e4fc4b6d00a15bfa499648a87d51c22940f70"): {Sym: "SUPER", Dec: 18},
	}
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	flag.IntVar(&v2NeedDistinct, "need", 10, "сколько разных базовых активов получить прежде чем выйти")
	flag.BoolVar(&v2Once, "once", true, "выход после получения нужного количества базовых активов")
	flag.IntVar(&v2EventLimit, "events", 0, "выход после N принятых событий цены (после 1bp фильтра)")
	flag.Parse()

	// Автозагрузка .env (не падаем если отсутствует)
	v2LoadDotEnv(".env")

	if err := v2LoadPools(); err != nil {
		log.Fatalf("load pools: %v", err)
	}
	if len(v2Pools) == 0 {
		log.Fatalf("no v2 pools loaded")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	v2RunLoop(sigCh)
}

// v2RunLoop поддерживает постоянное подключение с авто-реконнектом.
func v2RunLoop(sigCh <-chan os.Signal) {
	addresses := make([]string, 0, len(v2Pools))
	for a := range v2Pools {
		addresses = append(addresses, a.Hex())
	}
	log.Printf("[INFO] total V2 pool addresses=%d", len(addresses))
	if len(addresses) > 0 {
		maxShow := 5
		if len(addresses) < maxShow {
			maxShow = len(addresses)
		}
		log.Printf("[INFO] sample addresses: %s", strings.Join(addresses[:maxShow], ", "))
	}
	backoff := time.Second
	const backoffMax = 15 * time.Second
	attempt := 0
	interrupted := false
	for !interrupted {
		select {
		case <-sigCh:
			log.Printf("[INFO] interrupt (outer loop)")
			return
		default:
		}
		wsURL, err := v2ResolveWS()
		if err != nil {
			log.Printf("[ERR ] resolve ws: %v", err)
			time.Sleep(backoff)
			continue
		}
		log.Printf("[INFO] dial attempt=%d url=%s", attempt, v2MaskWS(wsURL))
		dialer := *websocket.DefaultDialer
		dialer.EnableCompression = true
		conn, _, err := dialer.Dial(wsURL, nil)
		if err != nil {
			log.Printf("[ERR ] dial failed: %v", err)
			time.Sleep(backoff)
			backoff = v2NextBackoff(backoff, backoffMax)
			attempt++
			continue
		}
		log.Printf("[INFO] WS connected attempt=%d", attempt)
		if backoff != time.Second {
			backoff = time.Second
		}
		attempt = 0

		ctx, cancel := context.WithCancel(context.Background())
		msgs := make(chan []byte, 1024)
		errs := make(chan error, 1)
		go v2Ping(ctx, conn)
		go v2Reader(conn, msgs, errs)
		if err := v2SubscribeAll(conn, addresses); err != nil {
			log.Printf("[ERR ] subscribe: %v", err)
			conn.Close()
			cancel()
			time.Sleep(backoff)
			backoff = v2NextBackoff(backoff, backoffMax)
			continue
		}
		timer := time.NewTimer(v2Timeout)

		run := true
		for run {
			select {
			case raw, ok := <-msgs:
				if !ok {
					log.Printf("[INFO] reader closed (will reconnect)")
					run = false
					continue
				}
				v2Handle(raw)
			case err := <-errs:
				if err != nil {
					if strings.Contains(err.Error(), "1006") || strings.Contains(strings.ToLower(err.Error()), "abnormal") || strings.Contains(err.Error(), "EOF") {
						log.Printf("[WARN] ws closed abnormal: %v (reconnect)", err)
					} else {
						log.Printf("[ERR ] ws error: %v", err)
					}
					run = false
				}
			case <-sigCh:
				log.Printf("[INFO] interrupt (inner loop) closing")
				interrupted = true
				run = false
			case <-timer.C:
				log.Printf("[INFO] timeout reached (reconnect)")
				run = false
			}
		}
		timer.Stop()
		cancel()
		conn.Close()
		if interrupted {
			log.Printf("[INFO] interrupted -> exit main loop")
			return
		}
		backoff = v2NextBackoff(backoff, backoffMax)
		log.Printf("[INFO] reconnect in %s", backoff)
		time.Sleep(backoff)
	}
}

func v2SubscribeAll(conn *websocket.Conn, addresses []string) error {
	batches := 0
	idCounter := 1
	for start := 0; start < len(addresses); start += v2BatchSize {
		end := start + v2BatchSize
		if end > len(addresses) {
			end = len(addresses)
		}
		part := addresses[start:end]
		sub := v2RPCRequest{JSONRPC: "2.0", ID: idCounter, Method: "eth_subscribe", Params: []interface{}{"logs", map[string]interface{}{"address": part}}}
		if err := conn.WriteJSON(sub); err != nil {
			return fmt.Errorf("subscribe batch %d: %w", batches, err)
		}
		log.Printf("[INFO] subscribe batch=%d id=%d size=%d", batches, idCounter, len(part))
		if len(part) <= 5 {
			log.Printf("[DEBUG] batch addresses: %s", strings.Join(part, ", "))
		}
		batches++
		idCounter++
	}
	log.Printf("[INFO] total subscribe batches=%d", batches)
	return nil
}

func v2NextBackoff(cur, max time.Duration) time.Duration {
	n := cur * 2
	if n > max {
		return max
	}
	return n
}

// --- Загрузка пулов ---
func v2LoadPools() error {
	path := os.Getenv("GECKO_POOLS_JSON")
	if path == "" {
		path = v2GeckoDefaultPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var f v2GeckoFile
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	rpcClient, err := v2DialRPC()
	if err != nil {
		log.Printf("[WARN] skip pair introspection rpc=%v", err)
	}
	if rpcClient != nil {
		defer rpcClient.Close()
	}

	loaded := 0
	for _, e := range f.Entries {
		if !strings.EqualFold(e.Dex, "uniswap_v2") {
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
		a0Lower := strings.ToLower(e.Token0.Address)
		a1Lower := strings.ToLower(e.Token1.Address)
		s0 := strings.ToUpper(e.Token0.Symbol)
		s1 := strings.ToUpper(e.Token1.Symbol)
		if s0 == "" {
			if sym, ok := v2StableAddr[strings.ToLower(e.Token0.Address)]; ok {
				s0 = sym
			}
		}
		if s1 == "" {
			if sym, ok := v2StableAddr[strings.ToLower(e.Token1.Address)]; ok {
				s1 = sym
			}
		}
		if s0 == "" && strings.EqualFold(e.Token0.Address, v2WETHAddress) {
			s0 = "WETH"
		}
		if s1 == "" && strings.EqualFold(e.Token1.Address, v2WETHAddress) {
			s1 = "WETH"
		}
		if s0 == "" {
			s0 = v2ShortAddr(e.Token0.Address)
		}
		if s1 == "" {
			s1 = v2ShortAddr(e.Token1.Address)
		}
		d0 := int(e.Token0.Decimals)
		d1 := int(e.Token1.Decimals)
		if can, ok := v2Canonical[a0Lower]; ok {
			s0 = can.Sym
			if d0 == 0 {
				d0 = can.Dec
			}
		}
		if can, ok := v2Canonical[a1Lower]; ok {
			s1 = can.Sym
			if d1 == 0 {
				d1 = can.Dec
			}
		}
		if d0 == 0 {
			if strings.EqualFold(e.Token0.Address, v2WETHAddress) {
				d0 = 18
			} else if v2Stable[s0] {
				if s0 == "USDC" || s0 == "USDT" {
					d0 = 6
				} else {
					d0 = 18
				}
			} else {
				d0 = 18
			}
		}
		if d1 == 0 {
			if strings.EqualFold(e.Token1.Address, v2WETHAddress) {
				d1 = 18
			} else if v2Stable[s1] {
				if s1 == "USDC" || s1 == "USDT" {
					d1 = 6
				} else {
					d1 = 18
				}
			} else {
				d1 = 18
			}
		}
		addr := common.HexToAddress(addrHex)
		pm := &v2PoolMeta{Addr: addr, PairName: e.PairName, Symbol0: s0, Symbol1: s1, Address0: common.HexToAddress(e.Token0.Address), Address1: common.HexToAddress(e.Token1.Address), Decimals0: d0, Decimals1: d1}
		if rpcClient != nil {
			if err := v2EnsurePoolOrder(pm, rpcClient); err != nil {
				log.Printf("[WARN] pool order pair=%s addr=%s err=%v", e.PairName, addr.Hex(), err)
			}
		}
		stable0 := v2Stable[pm.Symbol0]
		stable1 := v2Stable[pm.Symbol1]
		hasWETH0 := strings.EqualFold(pm.Address0.Hex(), v2WETHAddress)
		hasWETH1 := strings.EqualFold(pm.Address1.Hex(), v2WETHAddress)
		if stable0 && stable1 {
			continue
		}
		if !stable0 && !stable1 && !hasWETH0 && !hasWETH1 {
			continue
		}
		pm.HasStable = stable0 || stable1
		pm.HasWETH = hasWETH0 || hasWETH1
		pm.StableSym = ""
		if stable0 {
			pm.StableSym = pm.Symbol0
		} else if stable1 {
			pm.StableSym = pm.Symbol1
		}
		if pm.HasWETH && pm.HasStable { // WETH/Stable -> base=WETH
			if hasWETH0 && stable1 {
				pm.BaseIs0 = true
			} else if stable0 && hasWETH1 {
				pm.BaseIs0 = false
			} else if hasWETH0 {
				pm.BaseIs0 = true
			} else {
				pm.BaseIs0 = false
			}
		} else if pm.HasStable && !pm.HasWETH { // Token/Stable -> base=Token
			pm.BaseIs0 = !stable0
		} else if pm.HasWETH && !pm.HasStable { // Token/WETH -> base=Token
			pm.BaseIs0 = !hasWETH0
		} else {
			pm.BaseIs0 = true
		}
		v2Pools[addr] = pm
		loaded++
		if loaded <= 30 {
			log.Printf("[INFO] add V2 pool %s addr=%s cat=%s base=%s quote=%s", e.PairName, addr.Hex(), v2DescribePool(pm), v2Base(pm), v2Quote(pm))
		}
	}
	log.Printf("[INFO] total V2 pools loaded=%d", loaded)
	// Добавляем hard reference WETH/Stable если не были в JSON
	added := 0
	for _, hw := range v2HardWethStable {
		addr := common.HexToAddress(hw.Addr)
		if _, ok := v2Pools[addr]; ok {
			continue
		}
		stable := hw.Stable
		stAddr := common.HexToAddress(v2StableAddressForSymbol(stable))
		// Construct with real token0/token1 ordering
		var pm *v2PoolMeta
		switch stable {
		case "USDC": // token0=USDC, token1=WETH
			pm = &v2PoolMeta{Addr: addr, PairName: "USDC / WETH (hard)", Symbol0: "USDC", Symbol1: "WETH", Address0: stAddr, Address1: common.HexToAddress(v2WETHAddress), Decimals0: 6, Decimals1: 18, BaseIs0: false, HasStable: true, HasWETH: true, StableSym: stable}
		case "USDT": // token0=WETH, token1=USDT
			pm = &v2PoolMeta{Addr: addr, PairName: "WETH / USDT (hard)", Symbol0: "WETH", Symbol1: "USDT", Address0: common.HexToAddress(v2WETHAddress), Address1: stAddr, Decimals0: 18, Decimals1: 6, BaseIs0: true, HasStable: true, HasWETH: true, StableSym: stable}
		case "DAI": // token0=DAI, token1=WETH
			pm = &v2PoolMeta{Addr: addr, PairName: "DAI / WETH (hard)", Symbol0: "DAI", Symbol1: "WETH", Address0: stAddr, Address1: common.HexToAddress(v2WETHAddress), Decimals0: 18, Decimals1: 18, BaseIs0: false, HasStable: true, HasWETH: true, StableSym: stable}
		default:
			continue
		}
		v2Pools[addr] = pm
		added++
		log.Printf("[INFO] add HARD %s pool addr=%s", pm.PairName, addr.Hex())
	}
	if added > 0 {
		log.Printf("[INFO] hard reference pools added=%d", added)
	}
	return nil
}

func v2StableAddressForSymbol(sym string) string {
	up := strings.ToUpper(sym)
	switch up {
	case "USDC":
		return "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"
	case "USDT":
		return "0xdac17f958d2ee523a2206206994597c13d831ec7"
	case "DAI":
		return "0x6b175474e89094c44da98b954eedeac495271d0f"
	case "TUSD":
		return "0x0000000000085d4780B73119b644AE5ecd22b376"
	}
	return ""
}

// --- WS utils ---
func v2ResolveWS() (string, error) {
	if direct := strings.TrimSpace(os.Getenv("ALCHEMY_WS_URL")); direct != "" {
		return direct, nil
	}
	key := strings.TrimSpace(os.Getenv("ALCHEMY_API_KEY"))
	if key == "" {
		return "", fmt.Errorf("set ALCHEMY_WS_URL or ALCHEMY_API_KEY")
	}
	return fmt.Sprintf(v2DefaultMainnetTemplate, key), nil
}

func v2ResolveHTTP() (string, error) {
	if direct := strings.TrimSpace(os.Getenv("ALCHEMY_HTTP_URL")); direct != "" {
		return direct, nil
	}
	key := strings.TrimSpace(os.Getenv("ALCHEMY_API_KEY"))
	if key != "" {
		return fmt.Sprintf(v2DefaultHTTPTemplate, key), nil
	}
	if ws := strings.TrimSpace(os.Getenv("ALCHEMY_WS_URL")); ws != "" {
		if strings.HasPrefix(ws, "wss://") {
			return "https://" + ws[len("wss://"):], nil
		}
		if strings.HasPrefix(ws, "ws://") {
			return "http://" + ws[len("ws://"):], nil
		}
	}
	return "", fmt.Errorf("set ALCHEMY_HTTP_URL or ALCHEMY_API_KEY")
}

func v2DialRPC() (*rpc.Client, error) {
	url, err := v2ResolveHTTP()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return rpc.DialContext(ctx, url)
}

func v2EnsurePoolOrder(pm *v2PoolMeta, client *rpc.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	actual0, actual1, err := v2FetchPairTokens(ctx, client, pm.Addr)
	if err != nil {
		return err
	}
	if actual0 == pm.Address0 && actual1 == pm.Address1 {
		return nil
	}
	if actual0 == pm.Address1 && actual1 == pm.Address0 {
		v2SwapPoolMeta(pm)
		log.Printf("[INFO] adjust order pair=%s addr=%s token0=%s token1=%s", pm.PairName, pm.Addr.Hex(), pm.Symbol0, pm.Symbol1)
		return nil
	}
	return fmt.Errorf("tokens mismatch meta0=%s meta1=%s actual0=%s actual1=%s", pm.Address0.Hex(), pm.Address1.Hex(), actual0.Hex(), actual1.Hex())
}

func v2FetchPairTokens(ctx context.Context, client *rpc.Client, pair common.Address) (common.Address, common.Address, error) {
	t0, err := v2CallAddress(ctx, client, pair, "0x0dfe1681")
	if err != nil {
		return common.Address{}, common.Address{}, err
	}
	t1, err := v2CallAddress(ctx, client, pair, "0xd21220a7")
	if err != nil {
		return common.Address{}, common.Address{}, err
	}
	return t0, t1, nil
}

func v2CallAddress(ctx context.Context, client *rpc.Client, pair common.Address, data string) (common.Address, error) {
	var result string
	call := map[string]string{"to": pair.Hex(), "data": data}
	if err := client.CallContext(ctx, &result, "eth_call", call, "latest"); err != nil {
		return common.Address{}, err
	}
	return v2ParseAddressResult(result)
}

func v2ParseAddressResult(res string) (common.Address, error) {
	res = strings.TrimSpace(res)
	if !strings.HasPrefix(res, "0x") {
		return common.Address{}, fmt.Errorf("unexpected call result=%s", res)
	}
	b, err := hex.DecodeString(strings.TrimPrefix(res, "0x"))
	if err != nil {
		return common.Address{}, err
	}
	if len(b) < 32 {
		return common.Address{}, fmt.Errorf("result length=%d", len(b))
	}
	return common.BytesToAddress(b[12:]), nil
}

func v2SwapPoolMeta(pm *v2PoolMeta) {
	pm.Address0, pm.Address1 = pm.Address1, pm.Address0
	pm.Symbol0, pm.Symbol1 = pm.Symbol1, pm.Symbol0
	pm.Decimals0, pm.Decimals1 = pm.Decimals1, pm.Decimals0
	pm.LastPrice = nil
}

func v2Reader(c *websocket.Conn, out chan<- []byte, errs chan<- error) {
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

func v2Ping(ctx context.Context, c *websocket.Conn) {
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

func v2Handle(raw []byte) {
	if ack, ok := v2TryAck(raw); ok {
		if ack.Error != nil {
			log.Printf("[ERR ] subscribe %d %s", ack.Error.Code, ack.Error.Message)
		} else {
			log.Printf("[INFO] subscribed id=%s", ack.Result)
		}
		return
	}
	note, ok := v2TryNote(raw)
	if !ok {
		return
	}
	v2Process(note.Params.Result)
}

func v2TryAck(raw []byte) (*v2SubAck, bool) {
	var a v2SubAck
	if json.Unmarshal(raw, &a) != nil {
		return nil, false
	}
	if a.Result == "" && a.Error == nil {
		return nil, false
	}
	return &a, true
}
func v2TryNote(raw []byte) (*v2SubNote, bool) {
	var n v2SubNote
	if json.Unmarshal(raw, &n) != nil {
		return nil, false
	}
	if !strings.EqualFold(n.Method, "eth_subscription") {
		return nil, false
	}
	return &n, true
}

// --- Обработка логов ---
func v2Process(l v2LogItem) {
	if l.Removed || len(l.Topics) == 0 {
		return
	}
	if common.HexToHash(l.Topics[0]) != v2SyncSig {
		return
	} // интересует только Sync
	addr := common.HexToAddress(l.Address)
	meta, ok := v2Pools[addr]
	if !ok {
		return
	}
	// Data: reserve0 (uint112) + reserve1 (uint112) упакованы как 32 + 32 байт (ABI) => 64 hex после 0x
	if len(l.Data) < 2+64*2 { // 0x + 64 bytes *2 hex chars
		return
	}
	// Берём первые 32 байта -> reserve0, вторые 32 -> reserve1
	raw, err := hex.DecodeString(strings.TrimPrefix(l.Data, "0x"))
	if err != nil || len(raw) < 64 {
		return
	}
	r0 := new(big.Int).SetBytes(raw[0:32])
	r1 := new(big.Int).SetBytes(raw[32:64])
	price := v2Price(meta, r0, r1)
	v2Publish(meta, price, l, r0, r1)
}

// --- Расчёт цены ---
func v2Price(meta *v2PoolMeta, r0, r1 *big.Int) *big.Rat {
	if r0.Sign() == 0 || r1.Sign() == 0 {
		return nil
	}
	// price = reserveQuote/10^decQuote / (reserveBase/10^decBase)
	if meta.BaseIs0 {
		return v2Ratio(r1, meta.Decimals1, r0, meta.Decimals0)
	}
	return v2Ratio(r0, meta.Decimals0, r1, meta.Decimals1)
}

func v2Ratio(num *big.Int, numDec int, den *big.Int, denDec int) *big.Rat {
	if den.Sign() == 0 {
		return nil
	}
	nr := new(big.Rat).SetFrac(num, v2TenPow(uint(numDec)))
	dr := new(big.Rat).SetFrac(den, v2TenPow(uint(denDec)))
	return new(big.Rat).Quo(nr, dr)
}

func v2TenPow(dec uint) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil)
}

// --- Публикация (лог) ---
func v2Publish(meta *v2PoolMeta, price *big.Rat, l v2LogItem, r0, r1 *big.Int) {
	if price == nil {
		return
	}
	if meta.LastPrice != nil { // 1 bp фильтр
		delta := new(big.Rat).Sub(price, meta.LastPrice)
		if delta.Sign() == 0 {
			return
		}
		threshold := new(big.Rat).Quo(meta.LastPrice, big.NewRat(10000, 1))
		if delta.Abs(delta).Cmp(threshold) < 0 {
			return
		}
	}
	meta.LastPrice = new(big.Rat).Set(price)
	// Считаем событие
	v2EventsProcessed++
	if v2EventLimit > 0 && v2EventsProcessed >= v2EventLimit {
		defer func() { log.Printf("[INFO] events limit reached=%d exit", v2EventsProcessed); os.Exit(0) }()
	}
	base, quote := v2Base(meta), v2Quote(meta)
	// Если пул TOKEN-WETH – вместо цены в WETH показываем сразу USD через текущий WETHUSD.
	if meta.HasWETH && !meta.HasStable { // TOKEN-WETH
		if v2WETHUSD == nil {
			if !meta.GotFirst {
				meta.GotFirst = true
				v2CheckExit()
			}
			log.Printf("[WAIT] skip TOKEN-WETH pair=%s no WETHUSD yet block=%d", meta.PairName, v2Block(l.BlockNumber))
			return
		}
		// Явно определим где WETH и где TOKEN и пересчитаем USD: usdPerToken = (reserveWETH/10^decWETH)/(reserveTOKEN/10^decTOKEN) * WETHUSD
		var reserveToken, reserveWeth *big.Int
		var decToken, decWeth int
		tokenSymbol := v2TokenSymbol(meta)
		if strings.EqualFold(meta.Address0.Hex(), v2WETHAddress) { // token1 is token
			reserveWeth, reserveToken = r0, r1
			decWeth, decToken = meta.Decimals0, meta.Decimals1
		} else if strings.EqualFold(meta.Address1.Hex(), v2WETHAddress) { // token0 is token
			reserveToken, reserveWeth = r0, r1
			decToken, decWeth = meta.Decimals0, meta.Decimals1
		} else {
			return // защита — нет WETH фактически
		}
		if tokenDebug := strings.EqualFold(tokenSymbol, "SPX") || strings.EqualFold(tokenSymbol, "SUPER"); tokenDebug {
			log.Printf("[DBG ] token=%s raw_reserve_token=%s raw_reserve_weth=%s dec_token=%d dec_weth=%d", tokenSymbol, reserveToken.String(), reserveWeth.String(), decToken, decWeth)
		}
		if reserveToken.Sign() == 0 || reserveWeth.Sign() == 0 {
			return
		}
		tokenQty := new(big.Rat).SetFrac(reserveToken, v2TenPow(uint(decToken)))
		wethQty := new(big.Rat).SetFrac(reserveWeth, v2TenPow(uint(decWeth)))
		if tokenQty.Sign() == 0 {
			return
		}
		wethPerToken := new(big.Rat).Quo(wethQty, tokenQty)
		usdPerToken := new(big.Rat).Mul(wethPerToken, v2WETHUSD)
		// sanity: если > 100k USD токен – подозрительно, лог предупреждение и пропуск
		f, _ := new(big.Float).SetPrec(256).SetRat(usdPerToken).Float64()
		if f > 100000 {
			log.Printf("[WARN] skip unrealistic USD price token=%s usd=%s pair=%s", tokenSymbol, v2Format(usdPerToken, 8), meta.PairName)
			return
		}
		log.Printf("[PRICE] V2 pair=%s 1 %s = %s %s (USD via WETH) block=%d tx=%s", meta.PairName, tokenSymbol, v2Format(usdPerToken, 8), v2WETHUSDStable, v2Block(l.BlockNumber), l.TransactionHash)
	} else {
		pr := v2Format(price, 8)
		log.Printf("[PRICE] V2 pair=%s 1 %s = %s %s block=%d tx=%s", meta.PairName, base, pr, quote, v2Block(l.BlockNumber), l.TransactionHash)
	}

	// Обновим глобальную WETHUSD
	if meta.HasWETH && meta.HasStable {
		var wethUsd *big.Rat
		if (meta.BaseIs0 && strings.EqualFold(meta.Address0.Hex(), v2WETHAddress)) || (!meta.BaseIs0 && strings.EqualFold(meta.Address1.Hex(), v2WETHAddress)) {
			wethUsd = price
		} else {
			wethUsd = v2Invert(price)
		}
		if wethUsd != nil {
			if v2SanityWETHUSD(wethUsd) && v2ShouldUpdateWETH(meta.StableSym) {
				v2WETHUSD = new(big.Rat).Set(wethUsd)
				v2WETHUSDStable = meta.StableSym
				log.Printf("[USD ] WETHUSD=%s %s src=%s", v2Format(wethUsd, 8), meta.StableSym, meta.PairName)
			} else if !v2SanityWETHUSD(wethUsd) {
				log.Printf("[WARN] reject WETHUSD candidate=%s %s src=%s", v2Format(wethUsd, 8), meta.StableSym, meta.PairName)
			}
		}
	}

	if usd, stable, mode, ok := v2TokenUSD(meta, price); ok {
		// Для TOKEN-WETH мы уже сделали [PRICE] с USD – избегаем дублирования вторым логом
		if !(meta.HasWETH && !meta.HasStable) {
			log.Printf("[USD ] token=%s price=%s %s mode=%s block=%d", v2TokenSymbol(meta), v2Format(usd, 8), stable, mode, v2Block(l.BlockNumber))
		}
		if !meta.GotFirst {
			meta.GotFirst = true
			v2CheckExit()
		}
		return
	}
	if !meta.GotFirst {
		meta.GotFirst = true
		v2CheckExit()
	}
}

func v2CheckExit() {
	if !v2Once {
		return
	}
	// Нужно distinct базовых активов
	distinct := make(map[string]struct{})
	for _, pm := range v2Pools {
		if pm.GotFirst {
			distinct[v2Base(pm)] = struct{}{}
		}
	}
	got := len(distinct)
	log.Printf("[INFO] distinct bases so far=%d target=%d", got, v2NeedDistinct)
	if got >= v2NeedDistinct {
		log.Printf("[INFO] collected %d base prices. exit", got)
		os.Exit(0)
	}
}

// --- Helpers ---
func v2Base(m *v2PoolMeta) string {
	if m.BaseIs0 {
		return m.Symbol0
	}
	return m.Symbol1
}
func v2Quote(m *v2PoolMeta) string {
	if m.BaseIs0 {
		return m.Symbol1
	}
	return m.Symbol0
}

// --- USD helpers ---
func v2TokenSymbol(m *v2PoolMeta) string {
	if m.HasStable && !m.HasWETH { // Token/Stable
		if v2Stable[m.Symbol0] {
			return m.Symbol1
		}
		if v2Stable[m.Symbol1] {
			return m.Symbol0
		}
	}
	if m.HasWETH && !m.HasStable { // Token/WETH
		if strings.EqualFold(m.Address0.Hex(), v2WETHAddress) {
			return m.Symbol1
		}
		if strings.EqualFold(m.Address1.Hex(), v2WETHAddress) {
			return m.Symbol0
		}
	}
	return v2Base(m)
}

func v2Invert(r *big.Rat) *big.Rat {
	if r == nil || r.Sign() == 0 {
		return nil
	}
	return new(big.Rat).Inv(r)
}

func v2ShouldUpdateWETH(stable string) bool {
	if stable == "" {
		return false
	}
	if v2WETHUSD == nil {
		return true
	}
	if v2WETHUSDStable == "USDC" {
		return false
	}
	if v2WETHUSDStable == "USDT" && stable == "DAI" {
		return false
	}
	prio := func(s string) int {
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
	return prio(stable) > prio(v2WETHUSDStable)
}

// v2SanityWETHUSD проверяет что кандидат WETHUSD в разумном диапазоне.
// Быстрый фильтр: 300 < price < 100000 (учитывая исторические пики). Можно сузить до 500..10000.
// Дополнительно, если уже есть значение, отклоняем если отношение вне [0.5, 2].
func v2SanityWETHUSD(p *big.Rat) bool {
	if p == nil || p.Sign() <= 0 {
		return false
	}
	f, _ := new(big.Float).SetPrec(256).SetRat(p).Float64()
	if f < 300 || f > 100000 {
		return false
	}
	if v2WETHUSD != nil {
		curF, _ := new(big.Float).SetPrec(256).SetRat(v2WETHUSD).Float64()
		if curF > 0 {
			ratio := f / curF
			if ratio < 0.5 || ratio > 2.0 {
				return false
			}
		}
	}
	return true
}

func v2TokenUSD(meta *v2PoolMeta, price *big.Rat) (*big.Rat, string, string, bool) {
	if meta.HasWETH && meta.HasStable {
		return nil, "", "", false
	}
	if meta.HasStable && !meta.HasWETH {
		if v2Stable[v2Base(meta)] {
			return v2Invert(price), meta.StableSym, "direct-inv", true
		}
		return price, meta.StableSym, "direct", true
	}
	if meta.HasWETH && !meta.HasStable {
		if v2WETHUSD == nil {
			return nil, "", "", false
		}
		if v2IsTokenBase(meta) {
			usd := new(big.Rat).Mul(price, v2WETHUSD)
			return usd, v2WETHUSDStable, "derived", true
		}
		inv := v2Invert(price)
		if inv == nil {
			return nil, "", "", false
		}
		usd := new(big.Rat).Mul(inv, v2WETHUSD)
		return usd, v2WETHUSDStable, "derived-inv", true
	}
	return nil, "", "", false
}

func v2IsTokenBase(m *v2PoolMeta) bool {
	if m.HasWETH && !m.HasStable {
		if m.BaseIs0 && !strings.EqualFold(m.Address0.Hex(), v2WETHAddress) {
			return true
		}
		if !m.BaseIs0 && !strings.EqualFold(m.Address1.Hex(), v2WETHAddress) {
			return true
		}
	}
	if m.HasStable && !m.HasWETH {
		if m.BaseIs0 && !v2Stable[m.Symbol0] {
			return true
		}
		if !m.BaseIs0 && !v2Stable[m.Symbol1] {
			return true
		}
	}
	return false
}

func v2DescribePool(m *v2PoolMeta) string {
	switch {
	case m.HasWETH && m.HasStable:
		return "WETH-USD"
	case m.HasStable && !m.HasWETH:
		return "TOKEN-STABLE"
	case m.HasWETH && !m.HasStable:
		return "TOKEN-WETH"
	default:
		return "OTHER"
	}
}

func v2Format(r *big.Rat, prec int) string {
	f := new(big.Float).SetPrec(256).SetRat(r)
	return f.Text('f', prec)
}
func v2Block(h string) uint64 { bn, _ := hexutil.DecodeUint64(h); return bn }

// Маскирует ключ в ws URL
func v2MaskWS(u string) string {
	// ищем последний '/'
	i := strings.LastIndex(u, "/")
	if i == -1 || i+1 >= len(u) {
		return u
	}
	tail := u[i+1:]
	if len(tail) <= 6 {
		return u[:i+1] + "***"
	}
	return u[:i+1] + tail[:3] + "***" + tail[len(tail)-2:]
}

func v2ShortAddr(a string) string {
	a = strings.ToLower(a)
	a = strings.TrimPrefix(a, "0x")
	if len(a) < 8 {
		return a
	}
	return strings.ToUpper(a[:4] + "…" + a[len(a)-4:])
}

// --- .env loader (простой, без внешних зависимостей) ---
// v2LoadDotEnv читает файл построчно, парсит KEY=VALUE, пропуская комментарии и пустые строки.
// Не перезаписывает уже установленные переменные окружения.
func v2LoadDotEnv(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[INFO] .env not found path=%s (skip)", path)
		return
	}
	lines := bytes.Split(b, []byte("\n"))
	for idx, ln := range lines {
		s := strings.TrimSpace(string(ln))
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		eq := strings.Index(s, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(s[:eq])
		val := strings.TrimSpace(s[eq+1:])
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			log.Printf("[ERR ] set env line=%d key=%s err=%v", idx+1, key, err)
			continue
		}
		disp := val
		upKey := strings.ToUpper(key)
		if strings.Contains(upKey, "KEY") || strings.Contains(upKey, "SECRET") || strings.Contains(upKey, "TOKEN") || len(val) > 10 {
			if len(val) > 6 {
				disp = val[:3] + "***" + val[len(val)-2:]
			} else {
				disp = "***"
			}
		}
		log.Printf("[INFO] .env set %s=%s", key, disp)
	}
}
