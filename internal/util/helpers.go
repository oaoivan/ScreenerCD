package util

import "strings"

// BybitToGateSymbol converts Bybit spot symbol (e.g., BTCUSDT) to Gate format (BTC_USDT)
// If input is too short or not in expected pattern, returns as-is.
func BybitToGateSymbol(bybit string) string {
	if len(bybit) < 5 {
		return bybit
	}
	// naive split: assume USDT/USDC/DAI endings are common
	suffixes := []string{"USDT", "USDC", "DAI", "BUSD"}
	for _, s := range suffixes {
		if len(bybit) > len(s) && bybit[len(bybit)-len(s):] == s {
			base := bybit[:len(bybit)-len(s)]
			return base + "_" + s
		}
	}
	// default: insert underscore before last 4 (like USDT) if matches
	if len(bybit) > 4 {
		return bybit[:len(bybit)-4] + "_" + bybit[len(bybit)-4:]
	}
	return bybit
}

// NormalizeSpotSymbol normalizes raw spot symbol from any exchange to canonical BASEQUOTE form (e.g., BTC_USDT, BTC-USDT, BTC/USDT -> BTCUSDT).
// Only removes common separators and uppercases; assumes input already in BASE/QUOTE semantics.
func NormalizeSpotSymbol(exchange, raw string) string {
	s := strings.ToUpper(raw)
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	s = strings.ReplaceAll(s, "/", "")
	// Some venues might append spaces or extra dots; trim and drop dots
	s = strings.ReplaceAll(s, ".", "")
	s = strings.TrimSpace(s)
	return s
}

// AttachQuote формирует пары BASE+QUOTE (например, BTC + USDT -> BTCUSDT).
// Пустые значения отбрасываются, результат всегда в верхнем регистре.
func AttachQuote(bases []string, quote string) []string {
	normalizedQuote := strings.ToUpper(strings.TrimSpace(quote))
	if normalizedQuote == "" {
		// если котировка не указана, возвращаем копию исходного списка
		out := make([]string, 0, len(bases))
		for _, base := range bases {
			trimmed := strings.ToUpper(strings.TrimSpace(base))
			if trimmed == "" {
				continue
			}
			out = append(out, trimmed)
		}
		return out
	}

	out := make([]string, 0, len(bases))
	for _, base := range bases {
		trimmed := strings.ToUpper(strings.TrimSpace(base))
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed+normalizedQuote)
	}
	return out
}
