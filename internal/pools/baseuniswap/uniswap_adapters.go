package baseuniswap

import (
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	uniswap "github.com/yourusername/screner/internal/dex/Etherium/Uniswap"
	basepools "github.com/yourusername/screner/internal/pools/base"
)

// UniswapV3Pool describes a preprocessed V3 pool entry derived from base_pools.
type UniswapV3Pool struct {
	Dex         string
	Network     string
	PairName    string
	PoolID      common.Address
	PoolAddr    common.Address
	Token0      uniswap.TokenMeta
	Token1      uniswap.TokenMeta
	Fee         uint32
	TickSpacing int
	Hooks       common.Address
}

var defaultStableFallback = map[string]struct{}{
	"USDC": {},
	"USDT": {},
	"DAI":  {},
	"TUSD": {},
	"USD1": {},
	"USD":  {},
}

func ToUniswapV2Pool(entry basepools.Entry) (uniswap.PoolConfig, error) {
	version, _ := basepools.ParseAMMVersion(entry.AMMVersion)
	if version != basepools.VersionUnknown && version != basepools.VersionV2 {
		return uniswap.PoolConfig{}, fmt.Errorf("baseuniswap: entry amm_version=%s is not compatible with uniswap v2", entry.AMMVersion)
	}
	address, err := parseRequiredAddress(entry.PoolAddress)
	if err != nil {
		return uniswap.PoolConfig{}, fmt.Errorf("baseuniswap: pool address: %w", err)
	}
	token0, err := toTokenMeta(entry.Token0)
	if err != nil {
		return uniswap.PoolConfig{}, fmt.Errorf("baseuniswap: token0: %w", err)
	}
	token1, err := toTokenMeta(entry.Token1)
	if err != nil {
		return uniswap.PoolConfig{}, fmt.Errorf("baseuniswap: token1: %w", err)
	}

	pool := uniswap.PoolConfig{
		Address:      address,
		PairName:     normalizePair(entry.PairName, token0.Symbol, token1.Symbol),
		Token0:       token0,
		Token1:       token1,
		BaseIsToken0: chooseBase(entry, token0, token1),
	}
	return pool, nil
}

func ToUniswapV3Pool(entry basepools.Entry) (*UniswapV3Pool, error) {
	version, _ := basepools.ParseAMMVersion(entry.AMMVersion)
	if version != basepools.VersionUnknown && version != basepools.VersionV3 {
		return nil, fmt.Errorf("baseuniswap: entry amm_version=%s is not compatible with uniswap v3", entry.AMMVersion)
	}
	poolAddr, err := parseOptionalAddress(entry.PoolAddress)
	if err != nil {
		return nil, fmt.Errorf("baseuniswap: pool address: %w", err)
	}
	var poolID common.Address
	if addr := strings.TrimSpace(entry.PoolID); common.IsHexAddress(addr) {
		poolID = common.HexToAddress(addr)
	}
	if poolID == (common.Address{}) {
		poolID = poolAddr
	}
	if poolID == (common.Address{}) {
		return nil, fmt.Errorf("baseuniswap: pool id/address missing")
	}

	token0, err := toTokenMeta(entry.Token0)
	if err != nil {
		return nil, fmt.Errorf("baseuniswap: token0: %w", err)
	}
	token1, err := toTokenMeta(entry.Token1)
	if err != nil {
		return nil, fmt.Errorf("baseuniswap: token1: %w", err)
	}

	var fee uint32
	if entry.PoolKey.Fee != nil && *entry.PoolKey.Fee >= 0 {
		fee = uint32(*entry.PoolKey.Fee)
	}
	tickSpacing := 0
	if entry.PoolKey.TickSpacing != nil {
		tickSpacing = *entry.PoolKey.TickSpacing
	}
	hooks, _ := parseOptionalAddress(entry.PoolKey.Hooks)

	return &UniswapV3Pool{
		Dex:         entry.Dex,
		Network:     entry.Network,
		PairName:    normalizePair(entry.PairName, token0.Symbol, token1.Symbol),
		PoolID:      poolID,
		PoolAddr:    poolAddr,
		Token0:      token0,
		Token1:      token1,
		Fee:         fee,
		TickSpacing: tickSpacing,
		Hooks:       hooks,
	}, nil
}

func ToUniswapV4Pool(entry basepools.Entry) (uniswap.V4PoolConfig, common.Address, error) {
	version, _ := basepools.ParseAMMVersion(entry.AMMVersion)
	if version != basepools.VersionUnknown && version != basepools.VersionV4 {
		return uniswap.V4PoolConfig{}, common.Address{}, fmt.Errorf("baseuniswap: entry amm_version=%s is not compatible with uniswap v4", entry.AMMVersion)
	}
	manager, err := parseRequiredAddress(entry.PoolManager)
	if err != nil {
		return uniswap.V4PoolConfig{}, common.Address{}, fmt.Errorf("baseuniswap: pool_manager: %w", err)
	}
	poolHash, err := parseRequiredHash(firstNonEmpty(entry.PoolID, entry.PoolAddress))
	if err != nil {
		return uniswap.V4PoolConfig{}, common.Address{}, fmt.Errorf("baseuniswap: pool_id: %w", err)
	}
	token0, err := toTokenMeta(entry.Token0)
	if err != nil {
		return uniswap.V4PoolConfig{}, common.Address{}, fmt.Errorf("baseuniswap: token0: %w", err)
	}
	token1, err := toTokenMeta(entry.Token1)
	if err != nil {
		return uniswap.V4PoolConfig{}, common.Address{}, fmt.Errorf("baseuniswap: token1: %w", err)
	}
	hookAddr, _ := parseOptionalAddress(entry.PoolKey.Hooks)
	poolAddr, _ := parseOptionalAddress(entry.PoolAddress)

	pool := uniswap.V4PoolConfig{
		PoolID:        poolHash,
		PoolAddress:   poolAddr,
		HookAddress:   hookAddr,
		PairName:      normalizePair(entry.PairName, token0.Symbol, token1.Symbol),
		Token0:        token0,
		Token1:        token1,
		BaseIsToken0:  chooseBase(entry, token0, token1),
		CanonicalPair: "",
	}
	return pool, manager, nil
}

func toTokenMeta(token basepools.Token) (uniswap.TokenMeta, error) {
	addr, err := parseRequiredAddress(token.Address)
	if err != nil && !isZeroAddress(token.Address) {
		return uniswap.TokenMeta{}, err
	}
	meta := uniswap.TokenMeta{
		Address:  addr,
		Symbol:   normalizeSymbol(token.Symbol),
		Decimals: clampDecimals(token.Decimals),
	}
	if meta.Decimals == 0 {
		meta.Decimals = 18
	}
	if basepools.IsStableSymbol(meta.Symbol) || isFallbackStable(meta.Symbol) {
		meta.IsStable = true
	}
	if strings.EqualFold(meta.Symbol, "WETH") {
		meta.IsWETH = true
	}
	return meta, nil
}

func chooseBase(entry basepools.Entry, token0, token1 uniswap.TokenMeta) bool {
	baseAddr := strings.ToLower(strings.TrimSpace(entry.BaseToken))
	quoteAddr := strings.ToLower(strings.TrimSpace(entry.QuoteToken))
	token0Addr := strings.ToLower(token0.Address.Hex())
	token1Addr := strings.ToLower(token1.Address.Hex())

	switch {
	case baseAddr != "" && baseAddr == token0Addr:
		return true
	case baseAddr != "" && baseAddr == token1Addr:
		return false
	case quoteAddr != "" && quoteAddr == token0Addr:
		return false
	case quoteAddr != "" && quoteAddr == token1Addr:
		return true
	case token0.IsStable && !token1.IsStable:
		return false
	case token1.IsStable && !token0.IsStable:
		return true
	default:
		return true
	}
}

func normalizePair(current, symbol0, symbol1 string) string {
	name := strings.TrimSpace(current)
	if name == "" {
		name = fmt.Sprintf("%s/%s", strings.ToUpper(symbol0), strings.ToUpper(symbol1))
	}
	return strings.ToUpper(name)
}

func normalizeSymbol(sym string) string {
	return strings.ToUpper(strings.TrimSpace(sym))
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

func parseOptionalAddress(raw string) (common.Address, error) {
	addr := strings.TrimSpace(raw)
	if addr == "" {
		return common.Address{}, nil
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

func parseRequiredHash(raw string) (common.Hash, error) {
	val := strings.TrimSpace(raw)
	if val == "" {
		return common.Hash{}, fmt.Errorf("empty hash")
	}
	if !strings.HasPrefix(strings.ToLower(val), "0x") || len(val) != 66 {
		return common.Hash{}, fmt.Errorf("invalid hash %s", raw)
	}
	return common.HexToHash(val), nil
}

func clampDecimals(dec int) int {
	switch {
	case dec < 0:
		return 0
	case dec > 255:
		return 255
	default:
		return dec
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func isZeroAddress(raw string) bool {
	return strings.EqualFold(strings.TrimSpace(raw), common.Address{}.Hex())
}

func isFallbackStable(symbol string) bool {
	_, ok := defaultStableFallback[strings.ToUpper(strings.TrimSpace(symbol))]
	return ok
}
