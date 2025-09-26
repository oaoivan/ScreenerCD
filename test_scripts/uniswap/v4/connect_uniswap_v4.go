package main

// Отдельный бинарь для V4, избегаем конфликтов имён с V3 скриптом.

import (
	"bytes"
	"context"
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

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/gorilla/websocket"
)

const (
	v4DefaultMainnetTemplate = "wss://eth-mainnet.g.alchemy.com/v2/%s"
	v4PoolManagerABIPath     = "ABI/Uniswap/V4/UniswapV4PoolManager.json"
	v4Timeout                = 10 * time.Minute
)

type v4RPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type v4SubAck struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  string `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type v4SubNote struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		Subscription string    `json:"subscription"`
		Result       v4LogItem `json:"result"`
	} `json:"params"`
}

type v4LogItem struct {
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

type v4PoolEntry struct {
	Dex      string `json:"dex"`
	PairName string `json:"pair_name"`
	PoolID   string `json:"pool_id"`
	Network  string `json:"network"`
	Token0   struct {
		Address  string        `json:"address"`
		Symbol   string        `json:"symbol"`
		Decimals v4IntOrString `json:"decimals"`
	} `json:"token0"`
	Token1 struct {
		Address  string        `json:"address"`
		Symbol   string        `json:"symbol"`
		Decimals v4IntOrString `json:"decimals"`
	} `json:"token1"`
}

type v4GeckoFile struct {
	Entries []v4PoolEntry `json:"entries"`
}

type v4PoolMeta struct {
	PoolID    common.Hash
	PairName  string
	Symbol0   string
	Symbol1   string
	Addr0     common.Address
	Addr1     common.Address
	Decimals0 int
	Decimals1 int
	BaseIs0   bool
	LastPrice *big.Rat
	GotFirst  bool
}

// v4IntOrString позволяет парсить поле decimals как число или строку
type v4IntOrString int

func (v *v4IntOrString) UnmarshalJSON(b []byte) error {
	// trim spaces
	bb := bytes.TrimSpace(b)
	if len(bb) == 0 {
		*v = 0
		return nil
	}
	if bb[0] == '"' { // строка
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
		*v = v4IntOrString(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(bb, &n); err != nil {
		return err
	}
	*v = v4IntOrString(n)
	return nil
}

var (
	v4ABI       abi.ABI
	v4Events    map[common.Hash]abi.Event
	v4PoolByID  = make(map[common.Hash]*v4PoolMeta)
	v4Wanted    = []string{"UNI/USDC", "LINK/USDC"}
	v4TwoPow192 = new(big.Int).Lsh(big.NewInt(1), 192)
	v4Once      bool
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	flag.BoolVar(&v4Once, "once", false, "exit after first price for each target pool")
	flag.Parse()
	if err := v4LoadABI(); err != nil {
		log.Fatalf("ABI load: %v", err)
	}
	if err := v4LoadPools(); err != nil {
		log.Fatalf("load pools: %v", err)
	}
	if len(v4PoolByID) == 0 {
		log.Fatalf("no target pools found: %v", v4Wanted)
	}

	wsURL, err := v4ResolveWS()
	if err != nil {
		log.Fatalf("resolve ws: %v", err)
	}
	pmAddr := strings.TrimSpace(os.Getenv("POOLMANAGER_V4"))
	if pmAddr == "" {
		log.Fatalf("set POOLMANAGER_V4 env")
	}
	if !common.IsHexAddress(pmAddr) {
		log.Fatalf("invalid POOLMANAGER_V4: %s", pmAddr)
	}

	dialer := *websocket.DefaultDialer
	dialer.EnableCompression = true
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	log.Printf("[INFO] WS connected %s", wsURL)

	sub := v4RPCRequest{JSONRPC: "2.0", ID: 1, Method: "eth_subscribe", Params: []interface{}{"logs", map[string]interface{}{"address": []string{pmAddr}}}}
	if err := conn.WriteJSON(sub); err != nil {
		log.Fatalf("subscribe: %v", err)
	}
	js, _ := json.Marshal(sub)
	log.Printf("[INFO] subscribe sent: %s", string(js))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go v4Ping(ctx, conn)

	msgs := make(chan []byte, 128)
	errs := make(chan error, 1)
	go v4Reader(conn, msgs, errs)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	timer := time.NewTimer(v4Timeout)
	defer timer.Stop()

	for {
		select {
		case raw, ok := <-msgs:
			if !ok {
				log.Printf("[INFO] reader closed")
				return
			}
			v4Handle(raw)
		case err := <-errs:
			log.Printf("[ERR ] %v", err)
			return
		case <-sigCh:
			log.Printf("[INFO] interrupt")
			return
		case <-timer.C:
			log.Printf("[INFO] timeout")
			return
		}
	}
}

func v4LoadABI() error {
	b, err := os.ReadFile(v4PoolManagerABIPath)
	if err != nil {
		return err
	}
	parsed, err := abi.JSON(bytes.NewReader(b))
	if err != nil {
		return err
	}
	v4ABI = parsed
	v4Events = make(map[common.Hash]abi.Event)
	for _, e := range v4ABI.Events {
		v4Events[e.ID] = e
	}
	return nil
}

func v4LoadPools() error {
	path := os.Getenv("GECKO_POOLS_JSON")
	if path == "" {
		path = "Temp/geckoterminal_pools.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var g v4GeckoFile
	if err := json.Unmarshal(data, &g); err != nil {
		return err
	}
	wanted := make(map[string]struct{})
	for _, p := range v4Wanted {
		wanted[p] = struct{}{}
	}
	for _, e := range g.Entries {
		if !strings.EqualFold(e.Dex, "uniswap_v4") {
			continue
		}
		if _, ok := wanted[e.PairName]; !ok {
			continue
		}
		pid := strings.ToLower(strings.TrimSpace(e.PoolID))
		if !strings.HasPrefix(pid, "0x") || len(pid) != 66 {
			log.Printf("[WARN] skip invalid pool_id %s %s", e.PairName, e.PoolID)
			continue
		}
		poolID := common.HexToHash(pid)
		pm := &v4PoolMeta{PoolID: poolID, PairName: e.PairName, Symbol0: strings.ToUpper(e.Token0.Symbol), Symbol1: strings.ToUpper(e.Token1.Symbol), Addr0: common.HexToAddress(e.Token0.Address), Addr1: common.HexToAddress(e.Token1.Address), Decimals0: int(e.Token0.Decimals), Decimals1: int(e.Token1.Decimals)}
		// Автоопределение: если один из токенов стейбл, он становится quote.
		stable := map[string]bool{"USDC": true, "USDT": true, "DAI": true, "TUSD": true, "FDUSD": true, "USDD": true}
		if stable[pm.Symbol0] && !stable[pm.Symbol1] {
			pm.BaseIs0 = false
		} else if stable[pm.Symbol1] && !stable[pm.Symbol0] {
			pm.BaseIs0 = true
		} else {
			pm.BaseIs0 = true
		}
		v4PoolByID[poolID] = pm
		base, quote := v4Base(pm), v4Quote(pm)
		var decBase, decQuote int
		if pm.BaseIs0 {
			decBase, decQuote = pm.Decimals0, pm.Decimals1
		} else {
			decBase, decQuote = pm.Decimals1, pm.Decimals0
		}
		log.Printf("[INFO] added pool %s id=%s base=%s quote=%s decBase=%d decQuote=%d", e.PairName, poolID.Hex(), base, quote, decBase, decQuote)
		if len(v4PoolByID) == len(v4Wanted) {
			break
		}
	}
	return nil
}

func v4ResolveWS() (string, error) {
	if direct := strings.TrimSpace(os.Getenv("ALCHEMY_WS_URL")); direct != "" {
		return direct, nil
	}
	api := strings.TrimSpace(os.Getenv("ALCHEMY_API_KEY"))
	if api == "" {
		return "", fmt.Errorf("set ALCHEMY_WS_URL or ALCHEMY_API_KEY")
	}
	return fmt.Sprintf(v4DefaultMainnetTemplate, api), nil
}

func v4Reader(c *websocket.Conn, out chan<- []byte, errs chan<- error) {
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

func v4Ping(ctx context.Context, c *websocket.Conn) {
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

func v4Handle(raw []byte) {
	if ack, ok := v4TryAck(raw); ok {
		if ack.Error != nil {
			log.Printf("[ERR ] subscribe %d %s", ack.Error.Code, ack.Error.Message)
		} else {
			log.Printf("[INFO] subscribed id=%s", ack.Result)
		}
		return
	}
	note, ok := v4TryNote(raw)
	if !ok {
		return
	}
	v4Process(note.Params.Result)
}

func v4TryAck(raw []byte) (*v4SubAck, bool) {
	var a v4SubAck
	if json.Unmarshal(raw, &a) != nil {
		return nil, false
	}
	if a.Result == "" && a.Error == nil {
		return nil, false
	}
	return &a, true
}
func v4TryNote(raw []byte) (*v4SubNote, bool) {
	var n v4SubNote
	if json.Unmarshal(raw, &n) != nil {
		return nil, false
	}
	if !strings.EqualFold(n.Method, "eth_subscription") {
		return nil, false
	}
	return &n, true
}

func v4Process(l v4LogItem) {
	if l.Removed || len(l.Topics) == 0 {
		return
	}
	sig := common.HexToHash(l.Topics[0])
	evt, ok := v4Events[sig]
	if !ok || evt.Name != "Swap" {
		return
	}
	if len(l.Topics) < 2 {
		return
	}
	pid := common.HexToHash(l.Topics[1])
	meta, ok := v4PoolByID[pid]
	if !ok {
		return
	}
	fields := make(map[string]interface{})
	if l.Data != "0x" {
		b, err := hexutil.Decode(l.Data)
		if err != nil {
			return
		}
		if err := evt.Inputs.NonIndexed().UnpackIntoMap(fields, b); err != nil {
			return
		}
	}
	amount0, _ := v4Big(fields["amount0"])    // int128
	amount1, _ := v4Big(fields["amount1"])    // int128
	sqrtP, _ := v4Big(fields["sqrtPriceX96"]) // uint160
	liq, _ := v4Big(fields["liquidity"])      // uint128
	tick, _ := v4Big(fields["tick"])          // int24
	fee, _ := v4Big(fields["fee"])            // uint24
	price := v4Price(meta, amount0, amount1, sqrtP)
	v4Publish(meta, price, l, amount0, amount1, sqrtP, liq, tick, fee)
}

func v4Big(v interface{}) (*big.Int, bool) {
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

func v4Price(meta *v4PoolMeta, amount0, amount1, sqrtP *big.Int) *big.Rat {
	if sqrtP != nil && sqrtP.Sign() > 0 {
		sq := new(big.Int).Mul(sqrtP, sqrtP)
		raw := new(big.Rat).SetFrac(sq, v4TwoPow192)
		adj := new(big.Rat).SetFrac(v4TenPow(uint(meta.Decimals0)), v4TenPow(uint(meta.Decimals1)))
		p0over1 := new(big.Rat).Mul(raw, adj)
		if meta.BaseIs0 {
			return p0over1
		}
		return new(big.Rat).Inv(p0over1)
	}
	// fallback amounts
	if amount0 == nil || amount1 == nil {
		return nil
	}
	abs0 := v4Abs(amount0)
	abs1 := v4Abs(amount1)
	if meta.BaseIs0 {
		return v4Ratio(abs1, meta.Decimals1, abs0, meta.Decimals0)
	}
	return v4Ratio(abs0, meta.Decimals0, abs1, meta.Decimals1)
}

func v4Ratio(num *big.Int, numDec int, den *big.Int, denDec int) *big.Rat {
	if den.Sign() == 0 {
		return nil
	}
	nr := new(big.Rat).SetFrac(num, v4TenPow(uint(numDec)))
	dr := new(big.Rat).SetFrac(den, v4TenPow(uint(denDec)))
	return new(big.Rat).Quo(nr, dr)
}

func v4TenPow(dec uint) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil)
}
func v4Abs(v *big.Int) *big.Int {
	if v.Sign() < 0 {
		return new(big.Int).Neg(v)
	}
	return v
}

func v4Publish(meta *v4PoolMeta, price *big.Rat, l v4LogItem, a0, a1, sqrtP, liq, tick, fee *big.Int) {
	if price == nil {
		return
	}
	if meta.LastPrice != nil {
		delta := new(big.Rat).Sub(price, meta.LastPrice)
		if delta.Sign() == 0 {
			return
		}
		// 1 bp фильтр
		threshold := new(big.Rat).Quo(meta.LastPrice, big.NewRat(10000, 1))
		if delta.Abs(delta).Cmp(threshold) < 0 {
			return
		}
	}
	meta.LastPrice = new(big.Rat).Set(price)
	base, quote := v4Base(meta), v4Quote(meta)
	pr := v4Format(price, 8)
	log.Printf("[PRICE] pair=%s 1 %s = %s %s block=%d tx=%s", meta.PairName, base, pr, quote, v4Block(l.BlockNumber), l.TransactionHash)
	log.Printf("[SWAP] pair=%s sqrtP=%s a0=%s a1=%s liq=%s tick=%s fee=%s", meta.PairName, v4BigStr(sqrtP), v4BigStr(a0), v4BigStr(a1), v4BigStr(liq), v4BigStr(tick), v4BigStr(fee))
	if !meta.GotFirst {
		meta.GotFirst = true
		if v4Once {
			// проверяем все ли получили первую цену
			all := true
			left := 0
			for _, m := range v4PoolByID {
				if !m.GotFirst {
					all = false
					left++
				}
			}
			if all {
				log.Printf("[INFO] all target pools priced once. exit")
				os.Exit(0)
			}
			log.Printf("[INFO] got first price for %s, remaining=%d", meta.PairName, left)
		}
	}
}

func v4Base(m *v4PoolMeta) string {
	if m.BaseIs0 {
		return m.Symbol0
	}
	return m.Symbol1
}
func v4Quote(m *v4PoolMeta) string {
	if m.BaseIs0 {
		return m.Symbol1
	}
	return m.Symbol0
}

func v4Format(r *big.Rat, prec int) string {
	f := new(big.Float).SetPrec(256).SetRat(r)
	return f.Text('f', prec)
}
func v4BigStr(v *big.Int) string {
	if v == nil {
		return ""
	}
	return v.String()
}
func v4Block(h string) uint64 { bn, _ := hexutil.DecodeUint64(h); return bn }

// debug helper (unused now)
// (debug helper removed for once-mode build cleanliness)
