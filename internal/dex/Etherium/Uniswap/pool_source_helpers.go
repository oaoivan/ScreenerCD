package uniswap

import "strings"

func normalizeDexName(dex string) string {
	dex = strings.TrimSpace(strings.ToLower(dex))
	if dex == "" {
		return ""
	}
	dex = strings.ReplaceAll(dex, "-", "_")
	dex = strings.ReplaceAll(dex, " ", "_")
	return dex
}

func dexMatches(dex, filter string) bool {
	filterNorm := normalizeDexName(filter)
	if filterNorm == "" {
		return true
	}
	dexNorm := normalizeDexName(dex)
	if dexNorm == "" {
		return false
	}
	return strings.Contains(dexNorm, filterNorm)
}

func matchesAMMVersion(ammVersion, dex, want string) bool {
	want = strings.TrimSpace(strings.ToLower(want))
	if want == "" {
		return true
	}
	amm := strings.TrimSpace(strings.ToLower(ammVersion))
	if amm != "" {
		return amm == want
	}
	dexNorm := normalizeDexName(dex)
	if dexNorm == "" {
		return false
	}
	return strings.Contains(dexNorm, want)
}

func isUniswapDex(dex string) bool {
	dexNorm := normalizeDexName(dex)
	return strings.HasPrefix(dexNorm, "uniswap")
}
