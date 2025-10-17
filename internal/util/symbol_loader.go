package util

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"

	basepools "github.com/yourusername/screner/internal/pools/base"
)

// legacySymbolMap описывает устаревший формат {"BTC": {...}}.
type legacySymbolMap map[string]json.RawMessage

// stableSkipSet используется во fallback-парсере legacy JSON.
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

// LoadSymbolsFromFile возвращает список базовых символов для подписки CEX коннекторов.
// Приоритет — файл в формате base_pools. Для обратной совместимости поддерживается legacy JSON.
//
// Env-переменные:
//
//	SYMBOL_LOADER_AMM_FILTER       (пример: "v2,v3") — whitelist версий AMM, по умолчанию v1..v4.
//	SYMBOL_LOADER_INCLUDE_STABLE   (1/true)          — включить стейблкоины (по умолчанию исключаются).
func LoadSymbolsFromFile(filePath string) ([]string, error) {
	if symbols, err := loadFromBasePools(filePath); err == nil && len(symbols) > 0 {
		return symbols, nil
	}

	// fallback: legacy map format
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	if symbols, err := parseLegacySymbolMap(data); err == nil && len(symbols) > 0 {
		sort.Strings(symbols)
		return symbols, nil
	}
	return nil, errors.New("symbol_loader: unable to extract symbols from file")
}

func loadFromBasePools(path string) ([]string, error) {
	entries, err := basepools.LoadBasePools(path)
	if err != nil {
		return nil, err
	}
	filter := basepools.Filter{
		Versions: parseVersionWhitelist(),
	}
	filtered := basepools.FilterEntries(entries, filter)
	if len(filtered) == 0 {
		return nil, errors.New("symbol_loader: base_pools filter produced zero entries")
	}
	includeStable := includeStableSymbols()
	symbols := basepools.ExtractSymbols(filtered, !includeStable)
	if len(symbols) == 0 {
		return nil, errors.New("symbol_loader: base_pools extraction produced zero symbols")
	}
	sort.Strings(symbols)
	return symbols, nil
}

func parseVersionWhitelist() []basepools.Version {
	raw := strings.TrimSpace(os.Getenv("SYMBOL_LOADER_AMM_FILTER"))
	if raw == "" {
		return []basepools.Version{
			basepools.VersionV1,
			basepools.VersionV2,
			basepools.VersionV3,
			basepools.VersionV4,
		}
	}
	parts := strings.Split(raw, ",")
	result := make([]basepools.Version, 0, len(parts))
	for _, part := range parts {
		version, err := basepools.ParseAMMVersion(part)
		if err != nil {
			continue
		}
		result = append(result, version)
	}
	if len(result) == 0 {
		return []basepools.Version{
			basepools.VersionV1,
			basepools.VersionV2,
			basepools.VersionV3,
			basepools.VersionV4,
		}
	}
	return result
}

func includeStableSymbols() bool {
	val := strings.TrimSpace(os.Getenv("SYMBOL_LOADER_INCLUDE_STABLE"))
	if val == "" {
		return false
	}
	switch strings.ToLower(val) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func parseLegacySymbolMap(data []byte) ([]string, error) {
	var legacy legacySymbolMap
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, err
	}
	if len(legacy) == 0 {
		return nil, errors.New("legacy map is empty")
	}
	includeStable := includeStableSymbols()
	seen := make(map[string]struct{}, len(legacy))
	result := make([]string, 0, len(legacy))
	for ticker := range legacy {
		symbol := normalizeBaseSymbol(ticker)
		if symbol == "" {
			continue
		}
		if _, skip := stableSkipSet[symbol]; skip && !includeStable {
			continue
		}
		if _, exists := seen[symbol]; exists {
			continue
		}
		seen[symbol] = struct{}{}
		result = append(result, symbol)
	}
	if len(result) == 0 {
		return nil, errors.New("legacy map contains no valid symbols")
	}
	return result, nil
}

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
