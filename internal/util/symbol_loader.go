package util

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
)

// geckoTerminalFile описывает минимальную структуру файла с пулами GeckoTerminal.
type geckoTerminalFile struct {
	Entries []geckoTerminalEntry `json:"entries"`
}

// geckoTerminalEntry содержит только поле symbol, которое нам важно для подписки.
type geckoTerminalEntry struct {
	Symbol string `json:"symbol"`
}

// legacySymbolMap используется для обратной совместимости с файлами формата {"BTC": {...}}.
type legacySymbolMap map[string]json.RawMessage

// stableSkipSet хранит список базовых активов, которые не нужно подписывать.
var stableSkipSet = map[string]struct{}{
	"USDT": {},
	"USDC": {},
	"DAI":  {},
	"BUSD": {},
	"USD":  {},
	"EUR":  {},
}

// LoadSymbolsFromFile загружает базовые тикеры из JSON файла, поддерживая формат GeckoTerminal.
// Функция возвращает уникальные тикеры в верхнем регистре без привязки к котировке (например, "BTC").
// Stablecoins и пустые значения автоматически фильтруются. Порядок возвращаемых тикеров отсортирован.
func LoadSymbolsFromFile(filePath string) ([]string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	if symbols, err := parseGeckoEntries(data); err == nil && len(symbols) > 0 {
		sort.Strings(symbols)
		return symbols, nil
	}

	// Фолбэк на старый формат (map с ключами-тикерами), если entries отсутствует.
	legacySymbols, err := parseLegacySymbolMap(data)
	if err != nil {
		return nil, err
	}
	sort.Strings(legacySymbols)
	return legacySymbols, nil
}

// parseGeckoEntries разбирает массив entries и собирает уникальные тикеры.
func parseGeckoEntries(data []byte) ([]string, error) {
	var payload geckoTerminalFile
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	if len(payload.Entries) == 0 {
		return nil, errors.New("entries array is empty")
	}

	seen := make(map[string]struct{})
	result := make([]string, 0, len(payload.Entries))
	for _, entry := range payload.Entries {
		symbol := normalizeBaseSymbol(entry.Symbol)
		if symbol == "" {
			continue
		}
		if _, skip := stableSkipSet[symbol]; skip {
			continue
		}
		if _, exists := seen[symbol]; exists {
			continue
		}
		seen[symbol] = struct{}{}
		result = append(result, symbol)
	}
	if len(result) == 0 {
		return nil, errors.New("no valid symbols in entries")
	}
	return result, nil
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

// normalizeBaseSymbol приводит тикер к верхнему регистру и очищает от пробелов/разделителей.
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
