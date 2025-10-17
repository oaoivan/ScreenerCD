package basepools

import (
	"sort"
	"strings"
)

var stableSymbols = map[string]struct{}{
	"BUSD": {},
	"DAI":  {},
	"EUR":  {},
	"TUSD": {},
	"USD":  {},
	"USD1": {},
	"USDC": {},
	"USDT": {},
}

// FilterEntries возвращает срез записей, удовлетворяющих условию filter.
func FilterEntries(entries []Entry, filter Filter) []Entry {
	if len(entries) == 0 {
		return nil
	}
	if isEmptyFilter(filter) {
		out := make([]Entry, len(entries))
		copy(out, entries)
		return out
	}
	result := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.Matches(filter) {
			result = append(result, entry)
		}
	}
	return result
}

// NormalizeSymbolKey приводит символ к ключу без разделителей (аналог util.normalizeBaseSymbol).
func NormalizeSymbolKey(raw string) string {
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

// ExtractSymbols формирует отсортированный список уникальных символов.
// При skipStable=true исключаются стабильные активы.
func ExtractSymbols(entries []Entry, skipStable bool) []string {
	if len(entries) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(entries))
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		key := NormalizeSymbolKey(entry.Symbol)
		if key == "" {
			continue
		}
		if skipStable {
			if _, ok := stableSymbols[key]; ok {
				continue
			}
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func isEmptyFilter(f Filter) bool {
	return len(f.Dexes) == 0 &&
		len(f.Networks) == 0 &&
		len(f.Symbols) == 0 &&
		len(f.Versions) == 0 &&
		f.MinUSD <= 0 &&
		f.MaxUSD <= 0
}

// IsStableSymbol сообщает, считается ли символ стабильной монетой в контексте base_pools.
func IsStableSymbol(symbol string) bool {
	_, ok := stableSymbols[NormalizeSymbolKey(symbol)]
	return ok
}
