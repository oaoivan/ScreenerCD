package util

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// SymbolIdentity описывает комбинацию dex + amm_version + network.
type SymbolIdentity struct {
	Dex        string
	AMMVersion string
	Network    string
}

// Key возвращает нормализованный ключ идентичности.
func (id SymbolIdentity) Key() string {
	if id.Dex == "" || id.AMMVersion == "" || id.Network == "" {
		return ""
	}
	return id.Dex + "|" + id.AMMVersion + "|" + id.Network
}

// SymbolDescriptor связывает тикер с конкретной DEX/AMM/сетью и составным ключом пула.
type SymbolDescriptor struct {
	Symbol       string
	Identity     SymbolIdentity
	BaseToken    common.Address
	PoolAddress  common.Address
	Token0       PoolToken
	Token1       PoolToken
	PairName     string
	LiquidityUSD float64
	CompositeKey string
}

// PoolToken описывает токен внутри пула для построения ключей.
type PoolToken struct {
	Address common.Address
	Symbol  string
}

// legacySymbolMap используется для обратной совместимости с файлами формата {"BTC": {...}}.
type legacySymbolMap map[string]json.RawMessage

type basePoolFile struct {
	Entries []basePoolEntry `json:"entries"`
}

type basePoolEntry struct {
	Symbol       string        `json:"symbol"`
	Dex          string        `json:"dex"`
	AMMVersion   string        `json:"amm_version"`
	Network      string        `json:"network"`
	TokenAddress string        `json:"token_address"`
	PoolAddress  string        `json:"pool_address"`
	LiquidityUSD float64       `json:"liquidity_usd"`
	PairName     string        `json:"pair_name"`
	Token0       basePoolToken `json:"token0"`
	Token1       basePoolToken `json:"token1"`
}

type basePoolToken struct {
	Address string `json:"address"`
	Symbol  string `json:"symbol"`
}

// stableSkipSet хранит список базовых активов, которые не нужно подписывать.
var stableSkipSet = map[string]struct{}{
	"BUSD": {},
	"DAI":  {},
	"EUR":  {},
	"TUSD": {},
	"USD":  {},
	"USD1": {},
	"USDC": {},
	"USDT": {},
}

// LoadSymbolDescriptors возвращает структурированные данные по символам с учётом dex/amm/network.
func LoadSymbolDescriptors(filePath string) ([]SymbolDescriptor, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	return parseSymbolDescriptors(data)
}

// LoadSymbolsFromFile оставлен для обратной совместимости; при наличии entries
// использует структурированные дескрипторы и возвращает уникальные базы.
func LoadSymbolsFromFile(filePath string) ([]string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	if descriptors, err := parseSymbolDescriptors(data); err == nil && len(descriptors) > 0 {
		symbols := ExtractUniqueSymbols(descriptors)
		if len(symbols) == 0 {
			return nil, errors.New("no symbols derived from descriptors")
		}
		return symbols, nil
	}

	legacySymbols, err := parseLegacySymbolMap(data)
	if err != nil {
		return nil, err
	}
	sort.Strings(legacySymbols)
	return legacySymbols, nil
}

// FilterDescriptorsByIdentity оставляет только дескрипторы, подходящие под заданные комбинации.
func FilterDescriptorsByIdentity(descriptors []SymbolDescriptor, identities []SymbolIdentity) []SymbolDescriptor {
	if len(identities) == 0 {
		return descriptors
	}
	allowed := make(map[string]struct{}, len(identities))
	for _, id := range identities {
		if key := id.Key(); key != "" {
			allowed[key] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		return descriptors
	}
	filtered := make([]SymbolDescriptor, 0, len(descriptors))
	for _, desc := range descriptors {
		if _, ok := allowed[desc.Identity.Key()]; ok {
			filtered = append(filtered, desc)
		}
	}
	return filtered
}

// ExtractUniqueSymbols возвращает отсортированный список уникальных базовых символов.
func ExtractUniqueSymbols(descriptors []SymbolDescriptor) []string {
	if len(descriptors) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(descriptors))
	result := make([]string, 0, len(descriptors))
	for _, desc := range descriptors {
		if _, skip := stableSkipSet[desc.Symbol]; skip {
			continue
		}
		if _, ok := seen[desc.Symbol]; ok {
			continue
		}
		seen[desc.Symbol] = struct{}{}
		result = append(result, desc.Symbol)
	}
	sort.Strings(result)
	return result
}

func parseSymbolDescriptors(data []byte) ([]SymbolDescriptor, error) {
	var payload basePoolFile
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	if len(payload.Entries) == 0 {
		return nil, errors.New("entries array is empty")
	}

	bestByIdentity := make(map[string]SymbolDescriptor)
	skipped := 0
	countByDex := make(map[string]int)
	countByIdentity := make(map[string]int)

	for _, entry := range payload.Entries {
		symbol := normalizeBaseSymbol(entry.Symbol)
		if symbol == "" {
			continue
		}
		if _, skip := stableSkipSet[symbol]; skip {
			continue
		}

		identity := SymbolIdentity{
			Dex:        NormalizeSymbolDex(entry.Dex),
			AMMVersion: NormalizeSymbolAMM(entry.AMMVersion),
			Network:    NormalizeSymbolNetwork(entry.Network),
		}
		if identity.Dex == "" || identity.AMMVersion == "" || identity.Network == "" {
			skipped++
			continue
		}
		countByDex[identity.Dex]++
		countByIdentity[identity.Key()]++

		token0 := poolTokenFromEntry(entry.Token0)
		token1 := poolTokenFromEntry(entry.Token1)
		if !tokenHasIdentifier(token0) || !tokenHasIdentifier(token1) {
			skipped++
			continue
		}

		composite := buildCompositeKey(identity, token0, token1)
		if composite == "" {
			skipped++
			continue
		}

		baseAddr := resolveBaseTokenAddress(entry.TokenAddress, symbol, token0, token1)
		poolAddr := common.Address{}
		if common.IsHexAddress(entry.PoolAddress) {
			poolAddr = common.HexToAddress(entry.PoolAddress)
		}

		desc := SymbolDescriptor{
			Symbol:       symbol,
			Identity:     identity,
			BaseToken:    baseAddr,
			PoolAddress:  poolAddr,
			Token0:       token0,
			Token1:       token1,
			PairName:     strings.TrimSpace(entry.PairName),
			LiquidityUSD: entry.LiquidityUSD,
			CompositeKey: composite,
		}

		identityKey := identity.Key() + "|" + symbol
		if existing, ok := bestByIdentity[identityKey]; ok {
			if desc.LiquidityUSD > existing.LiquidityUSD {
				bestByIdentity[identityKey] = desc
			}
			continue
		}
		bestByIdentity[identityKey] = desc
	}

	if len(bestByIdentity) == 0 {
		return nil, errors.New("no valid symbol descriptors in entries")
	}

	result := make([]SymbolDescriptor, 0, len(bestByIdentity))
	for _, desc := range bestByIdentity {
		result = append(result, desc)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Symbol == result[j].Symbol {
			return result[i].Identity.Key() < result[j].Identity.Key()
		}
		return result[i].Symbol < result[j].Symbol
	})

	if skipped > 0 {
		Debugf("symbol_loader: skipped %d entries lacking identity or descriptor", skipped)
	}
	if len(result) > 0 {
		Debugf("symbol_loader: descriptors total=%d identities=%d", len(result), len(countByIdentity))
		dexKeys := make([]string, 0, len(countByDex))
		for dex := range countByDex {
			dexKeys = append(dexKeys, dex)
		}
		sort.Strings(dexKeys)
		for _, dex := range dexKeys {
			Debugf("symbol_loader: dex=%s descriptors=%d", dex, countByDex[dex])
		}
	}

	return result, nil
}

func poolTokenFromEntry(src basePoolToken) PoolToken {
	token := PoolToken{
		Symbol: strings.ToUpper(strings.TrimSpace(src.Symbol)),
	}
	if common.IsHexAddress(src.Address) {
		token.Address = common.HexToAddress(src.Address)
	}
	return token
}

func resolveBaseTokenAddress(raw string, symbol string, token0, token1 PoolToken) common.Address {
	if common.IsHexAddress(raw) {
		return common.HexToAddress(raw)
	}
	if strings.EqualFold(token0.Symbol, symbol) && token0.Address != (common.Address{}) {
		return token0.Address
	}
	if strings.EqualFold(token1.Symbol, symbol) && token1.Address != (common.Address{}) {
		return token1.Address
	}
	return common.Address{}
}

// parseLegacySymbolMap обрабатывает старый JSON формат, где ключи верхнего уровня являются тикерами.
func parseLegacySymbolMap(data []byte) ([]string, error) {
	var legacy legacySymbolMap
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, err
	}
	if len(legacy) == 0 {
		return nil, errors.New("no symbols found in legacy map")
	}
	seen := make(map[string]struct{}, len(legacy))
	symbols := make([]string, 0, len(legacy))
	for ticker := range legacy {
		symbol := normalizeBaseSymbol(ticker)
		if symbol == "" {
			continue
		}
		if _, skip := stableSkipSet[symbol]; skip {
			continue
		}
		if _, ok := seen[symbol]; ok {
			continue
		}
		seen[symbol] = struct{}{}
		symbols = append(symbols, symbol)
	}
	if len(symbols) == 0 {
		return nil, errors.New("no valid symbols in legacy map")
	}
	return symbols, nil
}

// NormalizeSymbolDex приводит dex к нижнему регистру и заменяет разделители.
func NormalizeSymbolDex(raw string) string {
	trimmed := strings.TrimSpace(strings.ToLower(raw))
	if trimmed == "" {
		return ""
	}
	trimmed = strings.ReplaceAll(trimmed, " ", "_")
	trimmed = strings.ReplaceAll(trimmed, "-", "_")
	trimmed = strings.ReplaceAll(trimmed, "__", "_")
	return trimmed
}

// StripDexVersion удаляет суффикс вида _v2/_v3 из нормализованного dex.
func StripDexVersion(raw string) string {
	norm := NormalizeSymbolDex(raw)
	if norm == "" {
		return ""
	}
	if idx := strings.Index(norm, "_v"); idx > 0 {
		return norm[:idx]
	}
	return norm
}

// NormalizeSymbolNetwork приводит название сети к нижнему регистру.
func NormalizeSymbolNetwork(raw string) string {
	trimmed := strings.TrimSpace(strings.ToLower(raw))
	if trimmed == "" {
		return ""
	}
	trimmed = strings.ReplaceAll(trimmed, " ", "_")
	trimmed = strings.ReplaceAll(trimmed, "-", "_")
	trimmed = strings.ReplaceAll(trimmed, "__", "_")
	return trimmed
}

// NormalizeSymbolAMM приводит amm_version к виду vX.
func NormalizeSymbolAMM(raw string) string {
	trimmed := strings.TrimSpace(strings.ToLower(raw))
	if trimmed == "" {
		return ""
	}
	if !strings.HasPrefix(trimmed, "v") {
		trimmed = "v" + trimmed
	}
	return trimmed
}

// normalizeBaseSymbol приводит тикер к верхнему регистру и очищает от разделителей.
func normalizeBaseSymbol(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	upper := strings.ToUpper(trimmed)
	upper = strings.ReplaceAll(upper, "-", "")
	upper = strings.ReplaceAll(upper, "_", "")
	upper = strings.ReplaceAll(upper, "/", "")
	return upper
}

func tokenIdentifier(token PoolToken) string {
	if token.Address != (common.Address{}) {
		return strings.ToLower(strings.TrimSpace(token.Address.Hex()))
	}
	return strings.ToLower(strings.TrimSpace(token.Symbol))
}

func tokenHasIdentifier(token PoolToken) bool {
	return tokenIdentifier(token) != ""
}

func buildCompositeKey(identity SymbolIdentity, token0, token1 PoolToken) string {
	key := identity.Key()
	if key == "" {
		return ""
	}
	id0 := tokenIdentifier(token0)
	id1 := tokenIdentifier(token1)
	if id0 == "" || id1 == "" {
		return ""
	}
	return key + "|" + id0 + "|" + id1
}
