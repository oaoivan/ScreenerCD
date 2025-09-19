package util

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