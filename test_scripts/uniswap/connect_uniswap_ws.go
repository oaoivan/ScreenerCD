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
	"unicode/utf8"

	geth "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/gorilla/websocket"
)

const (
	// defaultPoolIdentifier is the pool identifier we received (may be padded to 32 bytes).
	defaultPoolIdentifier  = "0x00b9edc1583bf6ef09ff3a09f6c23ecb57fd7d0bb75625717ec81eed181e22d7"
	poolABIPath            = "ABI/Uniswap/V3/UniswapV3Pool.json"
	defaultMainnetTemplate = "wss://eth-mainnet.g.alchemy.com/v2/%s"
	erc20ABIJSON           = `[
	{"constant":true,"inputs":[],"name":"symbol","outputs":[{"name":"","type":"string"}],"stateMutability":"view","type":"function"},
	{"constant":true,"inputs":[],"name":"decimals","outputs":[{"name":"","type":"uint8"}],"stateMutability":"view","type":"function"}
]`
	rpcTimeout = 15 * time.Second
)

// jsonRPCRequest is the shape of outgoing JSON-RPC subscription requests.
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

type decodedEvent struct {
	Name   string
	Fields map[string]interface{}
}

type tokenMetadata struct {
	Address  common.Address
	Symbol   string
	Decimals uint8
}

type poolMetadata struct {
	Pool               common.Address
	Token0             tokenMetadata
	Token1             tokenMetadata
	Base               tokenMetadata
	Quote              tokenMetadata
	BaseIsToken0       bool
	BaseSelectionGuess bool
}

var (
	pairABI        abi.ABI
	erc20ABI       abi.ABI
	eventBySig     map[common.Hash]abi.Event
	twoPow256      = new(big.Int).Lsh(big.NewInt(1), 256)
	twoPow192      = new(big.Int).Lsh(big.NewInt(1), 192)
	poolInfo       *poolMetadata
	symbolMethodID = []byte{0x95, 0xd8, 0x9b, 0x41}
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if err := initABI(); err != nil {
		log.Fatalf("unable to init ABI: %v", err)
	}

	wsURL, err := resolveAlchemyWSURL()
	if err != nil {
		log.Fatalf("unable to resolve Alchemy WS URL: %v", err)
	}

	poolAddress, rawIdentifier, derived, err := resolvePoolAddress()
	if err != nil {
		log.Fatalf("unable to resolve pool identifier: %v", err)
	}
	if derived {
		log.Printf("[INFO] derived pool contract %s from identifier %s", poolAddress, rawIdentifier)
	} else {
		log.Printf("[INFO] using pool contract %s", poolAddress)
	}

	metadata, err := loadPoolMetadata(context.Background(), wsURL, common.HexToAddress(poolAddress))
	if err != nil {
		log.Fatalf("unable to load pool metadata: %v", err)
	}
	poolInfo = metadata
	log.Printf("[INFO] pool token0=%s (%s, decimals=%d)", displaySymbol(metadata.Token0), metadata.Token0.Address.Hex(), metadata.Token0.Decimals)
	log.Printf("[INFO] pool token1=%s (%s, decimals=%d)", displaySymbol(metadata.Token1), metadata.Token1.Address.Hex(), metadata.Token1.Decimals)
	if metadata.BaseSelectionGuess {
		log.Printf("[INFO] base token guessed: %s (quote %s)", displaySymbol(metadata.Base), displaySymbol(metadata.Quote))
	} else {
		log.Printf("[INFO] base token set: %s (quote %s)", displaySymbol(metadata.Base), displaySymbol(metadata.Quote))
	}

	log.Printf("[INFO] connecting to Alchemy WS: %s", wsURL)
	dialer := *websocket.DefaultDialer
	dialer.EnableCompression = true
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		log.Fatalf("dial error: %v", err)
	}
	defer conn.Close()
	log.Printf("[INFO] connected")

	subReq := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "eth_subscribe",
		Params: []interface{}{
			"logs",
			map[string]interface{}{
				"address": []string{poolAddress},
			},
		},
	}

	if err := conn.WriteJSON(subReq); err != nil {
		log.Fatalf("subscribe write error: %v", err)
	}
	subPayload, _ := json.Marshal(subReq)
	log.Printf("[INFO] subscription request sent: %s", string(subPayload))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pingLoop(ctx, conn)
	messages := make(chan []byte, 16)
	errs := make(chan error, 1)

	go func() {
		defer close(messages)
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				errs <- fmt.Errorf("read error: %w", err)
				return
			}
			if msgType == websocket.PongMessage {
				log.Printf("[DBG ] pong received")
				continue
			}
			messages <- data
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	for {
		select {
		case data, ok := <-messages:
			if !ok {
				log.Printf("[INFO] reader closed")
				return
			}
			handleMessage(data)
		case err := <-errs:
			log.Printf("[ERR ] %v", err)
			return
		case <-time.After(2 * time.Minute):
			log.Printf("[INFO] timeout reached, shutting down")
			return
		case <-sigCh:
			log.Printf("[INFO] interrupt received, shutting down")
			return
		}
	}
}

func initABI() error {
	content, err := os.ReadFile(poolABIPath)
	if err != nil {
		return fmt.Errorf("read ABI: %w", err)
	}
	parsed, err := abi.JSON(bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("parse ABI: %w", err)
	}
	pairABI = parsed
	eventBySig = make(map[common.Hash]abi.Event)
	for _, event := range pairABI.Events {
		eventBySig[event.ID] = event
	}
	erc20Parsed, err := abi.JSON(strings.NewReader(erc20ABIJSON))
	if err != nil {
		return fmt.Errorf("parse ERC20 ABI: %w", err)
	}
	erc20ABI = erc20Parsed
	return nil
}

func resolveAlchemyWSURL() (string, error) {
	if direct := strings.TrimSpace(os.Getenv("ALCHEMY_WS_URL")); direct != "" {
		return direct, nil
	}
	apiKey := strings.TrimSpace(os.Getenv("ALCHEMY_API_KEY"))
	if apiKey == "" {
		return "", fmt.Errorf("set ALCHEMY_WS_URL or ALCHEMY_API_KEY environment variable")
	}
	return fmt.Sprintf(defaultMainnetTemplate, apiKey), nil
}

func resolvePoolAddress() (string, string, bool, error) {
	raw := strings.TrimSpace(os.Getenv("POOL_ADDRESS"))
	if raw == "" {
		raw = defaultPoolIdentifier
	}
	identifier := strings.TrimSpace(raw)
	if identifier == "" {
		return "", raw, false, fmt.Errorf("empty pool identifier")
	}
	if !strings.HasPrefix(identifier, "0x") {
		return "", raw, false, fmt.Errorf("pool identifier must start with 0x")
	}
	canon := strings.ToLower(identifier)
	hexPart := canon[2:]
	switch len(hexPart) {
	case 40:
		if _, err := hexutil.Decode(canon); err != nil {
			return "", raw, false, fmt.Errorf("invalid hex in pool identifier: %w", err)
		}
		return canon, raw, false, nil
	case 64:
		if _, err := hexutil.Decode(canon); err != nil {
			return "", raw, false, fmt.Errorf("invalid hex in pool identifier: %w", err)
		}
		hash := common.HexToHash(canon)
		addr := common.BytesToAddress(hash.Bytes()).Hex()
		return strings.ToLower(addr), raw, true, nil
	default:
		return "", raw, false, fmt.Errorf("unexpected identifier length: %d", len(hexPart))
	}
}

func loadPoolMetadata(ctx context.Context, rpcURL string, poolAddr common.Address) (*poolMetadata, error) {
	dialCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	client, err := ethclient.DialContext(dialCtx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("dial ethclient: %w", err)
	}
	defer client.Close()

	token0Addr, err := callPoolAddress(ctx, client, poolAddr, "token0")
	if err != nil {
		return nil, fmt.Errorf("fetch token0: %w", err)
	}
	token1Addr, err := callPoolAddress(ctx, client, poolAddr, "token1")
	if err != nil {
		return nil, fmt.Errorf("fetch token1: %w", err)
	}

	token0Meta, err := fetchTokenMetadata(ctx, client, token0Addr)
	if err != nil {
		return nil, fmt.Errorf("token0 metadata: %w", err)
	}
	token1Meta, err := fetchTokenMetadata(ctx, client, token1Addr)
	if err != nil {
		return nil, fmt.Errorf("token1 metadata: %w", err)
	}

	meta := &poolMetadata{
		Pool:   poolAddr,
		Token0: token0Meta,
		Token1: token1Meta,
	}
	base, quote, guessed, err := chooseBaseToken(meta)
	if err != nil {
		return nil, err
	}
	meta.Base = base
	meta.Quote = quote
	meta.BaseIsToken0 = meta.Base.Address == meta.Token0.Address
	meta.BaseSelectionGuess = guessed
	return meta, nil
}

func callPoolAddress(ctx context.Context, client *ethclient.Client, poolAddr common.Address, method string) (common.Address, error) {
	data, err := pairABI.Pack(method)
	if err != nil {
		return common.Address{}, fmt.Errorf("pack %s: %w", method, err)
	}
	resp, err := callContract(ctx, client, poolAddr, data)
	if err != nil {
		return common.Address{}, err
	}
	if len(resp) < 32 {
		return common.Address{}, fmt.Errorf("unexpected response size %d", len(resp))
	}
	return common.BytesToAddress(resp[12:32]), nil
}

func fetchTokenMetadata(ctx context.Context, client *ethclient.Client, addr common.Address) (tokenMetadata, error) {
	decimals, err := callERC20Decimals(ctx, client, addr)
	if err != nil {
		return tokenMetadata{}, err
	}
	symbol, symErr := callERC20Symbol(ctx, client, addr)
	if symErr != nil {
		log.Printf("[WARN] token %s symbol fetch failed: %v", addr.Hex(), symErr)
		symbol = ""
	}
	return tokenMetadata{Address: addr, Symbol: symbol, Decimals: decimals}, nil
}

func callERC20Decimals(ctx context.Context, client *ethclient.Client, addr common.Address) (uint8, error) {
	data, err := erc20ABI.Pack("decimals")
	if err != nil {
		return 0, fmt.Errorf("pack decimals: %w", err)
	}
	resp, err := callContract(ctx, client, addr, data)
	if err != nil {
		return 0, fmt.Errorf("call decimals: %w", err)
	}
	outputs, err := erc20ABI.Unpack("decimals", resp)
	if err != nil {
		return 0, fmt.Errorf("unpack decimals: %w", err)
	}
	if len(outputs) == 0 {
		return 0, fmt.Errorf("decimals empty response")
	}
	switch v := outputs[0].(type) {
	case uint8:
		return v, nil
	case *big.Int:
		return uint8(v.Uint64()), nil
	default:
		return 0, fmt.Errorf("unexpected decimals type %T", outputs[0])
	}
}

func callERC20Symbol(ctx context.Context, client *ethclient.Client, addr common.Address) (string, error) {
	data, err := erc20ABI.Pack("symbol")
	if err != nil {
		return "", fmt.Errorf("pack symbol: %w", err)
	}
	resp, err := callContract(ctx, client, addr, data)
	if err == nil {
		outputs, unpackErr := erc20ABI.Unpack("symbol", resp)
		if unpackErr == nil && len(outputs) > 0 {
			switch v := outputs[0].(type) {
			case string:
				return v, nil
			}
		}
	}
	// fallback to bytes32 signature 0x95d89b41
	resp, err = callContract(ctx, client, addr, symbolMethodID)
	if err != nil {
		return "", fmt.Errorf("call symbol bytes32: %w", err)
	}
	if len(resp) < 32 {
		return "", fmt.Errorf("unexpected symbol bytes length %d", len(resp))
	}
	trimmed := bytes.TrimRight(resp[:32], "\x00")
	if len(trimmed) == 0 {
		return "", nil
	}
	if utf8.Valid(trimmed) {
		return string(trimmed), nil
	}
	return hexutil.Encode(trimmed), nil
}

func callContract(ctx context.Context, client *ethclient.Client, to common.Address, data []byte) ([]byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	msg := geth.CallMsg{To: &to, Data: data}
	resp, err := client.CallContract(callCtx, msg, nil)
	if err != nil {
		return nil, fmt.Errorf("call contract: %w", err)
	}
	return resp, nil
}

func chooseBaseToken(meta *poolMetadata) (tokenMetadata, tokenMetadata, bool, error) {
	if meta == nil {
		return tokenMetadata{}, tokenMetadata{}, false, fmt.Errorf("nil metadata")
	}
	baseEnv := strings.TrimSpace(os.Getenv("BASE_TOKEN_ADDRESS"))
	if baseEnv != "" {
		normalized := strings.ToLower(baseEnv)
		switch normalized {
		case strings.ToLower(meta.Token0.Address.Hex()):
			return meta.Token0, meta.Token1, false, nil
		case strings.ToLower(meta.Token1.Address.Hex()):
			return meta.Token1, meta.Token0, false, nil
		default:
			return tokenMetadata{}, tokenMetadata{}, false, fmt.Errorf("BASE_TOKEN_ADDRESS %s does not match pool tokens", baseEnv)
		}
	}
	upper0 := strings.ToUpper(meta.Token0.Symbol)
	upper1 := strings.ToUpper(meta.Token1.Symbol)
	if strings.Contains(upper0, "ETH") && !strings.Contains(upper1, "ETH") {
		return meta.Token0, meta.Token1, true, nil
	}
	if strings.Contains(upper1, "ETH") && !strings.Contains(upper0, "ETH") {
		return meta.Token1, meta.Token0, true, nil
	}
	// default fallback to token1 as base if it looks like wrapped ETH, else token0
	if strings.Contains(upper1, "WETH") || strings.Contains(upper1, "ETH") {
		return meta.Token1, meta.Token0, true, nil
	}
	return meta.Token0, meta.Token1, true, nil
}

func pingLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				log.Printf("[ERR ] ping write: %v", err)
				return
			}
			log.Printf("[DBG ] ping sent")
		}
	}
}

func handleMessage(raw []byte) {
	if ack, ok := trySubscriptionAck(raw); ok {
		if ack.Error != nil {
			log.Printf("[ERR ] subscription failed: %d %s", ack.Error.Code, ack.Error.Message)
		} else {
			log.Printf("[INFO] subscription confirmed: %s", ack.Result)
		}
		return
	}

	note, ok := tryNotification(raw)
	if ok {
		decoded, err := decodeLog(note.Params.Result)
		if err != nil {
			log.Printf("[LOG ] decode error: %v", err)
			logRawJSON(raw)
			return
		}
		if processDecodedEvent(note.Params.Result, decoded) {
			return
		}
		printDecodedEvent(note.Params.Result, decoded)
		return
	}

	logRawJSON(raw)
}

func processDecodedEvent(result logResult, evt *decodedEvent) bool {
	if poolInfo != nil && evt.Name == "Swap" {
		if err := handleSwap(result, evt); err != nil {
			log.Printf("[SWAP] decode error: %v", err)
		}
		return true
	}
	return false
}

func handleSwap(result logResult, evt *decodedEvent) error {
	if poolInfo == nil {
		return fmt.Errorf("pool metadata not initialized")
	}
	amount0, err := requireBigInt(evt.Fields, "amount0")
	if err != nil {
		return err
	}
	amount1, err := requireBigInt(evt.Fields, "amount1")
	if err != nil {
		return err
	}
	sqrtPrice, err := requireBigInt(evt.Fields, "sqrtPriceX96")
	if err != nil {
		return err
	}
	liquidity, err := requireBigInt(evt.Fields, "liquidity")
	if err != nil {
		return err
	}
	tickValue, err := requireBigInt(evt.Fields, "tick")
	if err != nil {
		return err
	}
	tick := tickValue.Int64()

	var baseAmount, quoteAmount *big.Int
	if poolInfo.BaseIsToken0 {
		baseAmount = amount0
		quoteAmount = amount1
	} else {
		baseAmount = amount1
		quoteAmount = amount0
	}

	priceRat, err := priceFromAmounts(baseAmount, quoteAmount, poolInfo.Base.Decimals, poolInfo.Quote.Decimals)
	if err != nil {
		return fmt.Errorf("price from amounts: %w", err)
	}
	sqrtPriceRat := priceFromSqrt(sqrtPrice, poolInfo)

	baseAmountStr := formatTokenAmount(baseAmount, poolInfo.Base.Decimals, 6)
	quoteAmountStr := formatTokenAmount(quoteAmount, poolInfo.Quote.Decimals, 2)
	priceStr := formatRat(priceRat, 6)
	sqrtPriceStr := formatRat(sqrtPriceRat, 6)
	liquidityStr := formatTokenAmount(liquidity, poolInfo.Base.Decimals, 6)
	blockLabel := formatBlockNumber(result.BlockNumber)
	side := tradeSide(baseAmount)
	sender := stringFromField(evt.Fields["sender"], "?")
	recipient := stringFromField(evt.Fields["recipient"], "?")

	log.Printf("[SWAP] block=%s tx=%s side=%s price=%s %s/%s price_sqrt=%s base=%s %s quote=%s %s sqrtP=%s tick=%d liquidity=%s sender=%s recipient=%s",
		blockLabel,
		result.TransactionHash,
		side,
		priceStr,
		displaySymbol(poolInfo.Base),
		displaySymbol(poolInfo.Quote),
		sqrtPriceStr,
		displaySymbol(poolInfo.Base),
		baseAmountStr,
		displaySymbol(poolInfo.Quote),
		quoteAmountStr,
		sqrtPrice.String(),
		tick,
		liquidityStr,
		sender,
		recipient,
	)
	return nil
}

func requireBigInt(fields map[string]interface{}, key string) (*big.Int, error) {
	val, ok := fields[key]
	if !ok {
		return nil, fmt.Errorf("field %s missing", key)
	}
	switch v := val.(type) {
	case *big.Int:
		return v, nil
	case string:
		if strings.HasPrefix(v, "0x") {
			decoded, err := hexutil.DecodeBig(v)
			if err != nil {
				return nil, fmt.Errorf("hex decode %s: %w", key, err)
			}
			return decoded, nil
		}
	case common.Hash:
		return v.Big(), nil
	}
	return nil, fmt.Errorf("field %s unexpected type %T", key, val)
}

func priceFromAmounts(baseAmount, quoteAmount *big.Int, baseDecimals, quoteDecimals uint8) (*big.Rat, error) {
	if baseAmount == nil || quoteAmount == nil {
		return nil, fmt.Errorf("nil amounts")
	}
	baseAbs := absBigInt(baseAmount)
	quoteAbs := absBigInt(quoteAmount)
	if baseAbs.Sign() == 0 {
		return nil, fmt.Errorf("base amount is zero")
	}
	baseRat := new(big.Rat).SetFrac(baseAbs, tenPow(baseDecimals))
	quoteRat := new(big.Rat).SetFrac(quoteAbs, tenPow(quoteDecimals))
	return new(big.Rat).Quo(quoteRat, baseRat), nil
}

func priceFromSqrt(sqrtPrice *big.Int, meta *poolMetadata) *big.Rat {
	if sqrtPrice == nil || meta == nil {
		return nil
	}
	numerator := new(big.Int).Mul(sqrtPrice, sqrtPrice)
	raw := new(big.Rat).SetFrac(numerator, twoPow192)
	if raw.Sign() == 0 {
		return raw
	}
	if meta.BaseIsToken0 {
		adjust := decimalAdjust(meta.Token0.Decimals, meta.Token1.Decimals)
		return new(big.Rat).Mul(raw, adjust)
	}
	adjust := decimalAdjust(meta.Token1.Decimals, meta.Token0.Decimals)
	return new(big.Rat).Mul(new(big.Rat).Inv(raw), adjust)
}

func decimalAdjust(decimalsA, decimalsB uint8) *big.Rat {
	return new(big.Rat).SetFrac(tenPow(decimalsA), tenPow(decimalsB))
}

func tenPow(dec uint8) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil)
}

func formatTokenAmount(amount *big.Int, decimals uint8, precision int) string {
	if amount == nil {
		return "0"
	}
	sign := ""
	if amount.Sign() < 0 {
		sign = "-"
	}
	rat := new(big.Rat).SetFrac(absBigInt(amount), tenPow(decimals))
	return sign + formatRat(rat, precision)
}

func formatRat(r *big.Rat, precision int) string {
	if r == nil {
		return "?"
	}
	f := new(big.Float).SetPrec(256).SetRat(r)
	return f.Text('f', precision)
}

func absBigInt(v *big.Int) *big.Int {
	if v == nil {
		return big.NewInt(0)
	}
	if v.Sign() >= 0 {
		return new(big.Int).Set(v)
	}
	return new(big.Int).Neg(v)
}

func tradeSide(baseAmount *big.Int) string {
	if baseAmount == nil {
		return "?"
	}
	switch baseAmount.Sign() {
	case -1:
		return "BUY"
	case 1:
		return "SELL"
	default:
		return "FLAT"
	}
}

func stringFromField(val interface{}, def string) string {
	switch v := val.(type) {
	case string:
		return v
	case common.Address:
		return v.Hex()
	default:
		return def
	}
}

func formatBlockNumber(blockHex string) string {
	if bn, err := hexutil.DecodeUint64(blockHex); err == nil {
		return fmt.Sprintf("%d", bn)
	}
	return blockHex
}

func displaySymbol(meta tokenMetadata) string {
	if meta.Symbol != "" {
		return meta.Symbol
	}
	return meta.Address.Hex()
}

func trySubscriptionAck(raw []byte) (*subscriptionAck, bool) {
	var ack subscriptionAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		return nil, false
	}
	if ack.Result == "" && ack.Error == nil {
		return nil, false
	}
	return &ack, true
}

func tryNotification(raw []byte) (*subscriptionNotification, bool) {
	var note subscriptionNotification
	if err := json.Unmarshal(raw, &note); err != nil {
		return nil, false
	}
	if !strings.EqualFold(note.Method, "eth_subscription") {
		return nil, false
	}
	return &note, true
}

func decodeLog(result logResult) (*decodedEvent, error) {
	if len(result.Topics) == 0 {
		return nil, fmt.Errorf("log without topics")
	}
	sigHash := common.HexToHash(result.Topics[0])
	event, ok := eventBySig[sigHash]
	if !ok {
		return nil, fmt.Errorf("unknown event signature: %s", result.Topics[0])
	}
	fields := make(map[string]interface{})
	if payload := strings.TrimSpace(result.Data); payload != "" && payload != "0x" {
		dataBytes, err := hexutil.Decode(payload)
		if err != nil {
			return nil, fmt.Errorf("decode data: %w", err)
		}
		if len(event.Inputs.NonIndexed()) > 0 {
			if err := event.Inputs.NonIndexed().UnpackIntoMap(fields, dataBytes); err != nil {
				return nil, fmt.Errorf("unpack data: %w", err)
			}
		}
	}
	indexedValues, err := decodeIndexedArgs(event, result.Topics)
	if err != nil {
		return nil, err
	}
	for k, v := range indexedValues {
		fields[k] = v
	}
	return &decodedEvent{Name: event.Name, Fields: fields}, nil
}

func decodeIndexedArgs(event abi.Event, topics []string) (map[string]interface{}, error) {
	values := make(map[string]interface{})
	topicPos := 1 // topic[0] is the event signature hash
	for _, arg := range event.Inputs {
		if !arg.Indexed {
			continue
		}
		if topicPos >= len(topics) {
			return nil, fmt.Errorf("topic missing for argument %s", arg.Name)
		}
		val, err := decodeTopicValue(arg, topics[topicPos])
		if err != nil {
			return nil, fmt.Errorf("decode indexed %s: %w", arg.Name, err)
		}
		values[arg.Name] = val
		topicPos++
	}
	return values, nil
}

func decodeTopicValue(arg abi.Argument, topic string) (interface{}, error) {
	data, err := hexutil.Decode(topic)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty topic data")
	}
	switch arg.Type.T {
	case abi.AddressTy:
		return common.BytesToAddress(data[12:]).Hex(), nil
	case abi.UintTy:
		return new(big.Int).SetBytes(data), nil
	case abi.IntTy:
		return decodeSignedBigInt(data), nil
	case abi.BoolTy:
		return data[len(data)-1] == 1, nil
	case abi.FixedBytesTy:
		size := arg.Type.Size
		if size <= 0 || size > len(data) {
			size = len(data)
		}
		return hexutil.Encode(data[:size]), nil
	default:
		// Dynamic types (string/bytes/arrays) are hashed in topics; return raw topic
		return topic, nil
	}
}

func decodeSignedBigInt(data []byte) *big.Int {
	val := new(big.Int).SetBytes(data)
	if len(data) > 0 && data[0]&0x80 != 0 {
		val.Sub(val, twoPow256)
	}
	return val
}

func printDecodedEvent(result logResult, evt *decodedEvent) {
	blockLabel := formatBlockNumber(result.BlockNumber)
	log.Printf("[EVT ] block=%s tx=%s event=%s %s", blockLabel, result.TransactionHash, evt.Name, formatFields(evt.Fields))
}

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
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", key, normalizeValue(fields[key])))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func normalizeValue(value interface{}) string {
	switch v := value.(type) {
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
		return "false"
	case *big.Int:
		return v.String()
	case common.Address:
		return v.Hex()
	case common.Hash:
		return v.Hex()
	case []byte:
		return hexutil.Encode(v)
	case [32]byte:
		return hexutil.Encode(v[:])
	case [20]byte:
		return hexutil.Encode(v[:])
	default:
		if s, ok := v.(fmt.Stringer); ok {
			return s.String()
		}
		return fmt.Sprintf("%v", v)
	}
}

func logRawJSON(raw []byte) {
	var generic interface{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		log.Printf("[RAW ] %s", string(raw))
		return
	}
	pretty, err := json.Marshal(generic)
	if err != nil {
		log.Printf("[RAW ] %s", string(raw))
		return
	}
	log.Printf("[MSG ] %s", string(pretty))
}
