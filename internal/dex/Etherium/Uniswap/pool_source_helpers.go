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

func dexMatchesAny(dex string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, filter := range filters {
		if dexMatches(dex, filter) {
			return true
		}
	}
	return false
}

func normalizeNetworkName(network string) string {
	network = strings.TrimSpace(strings.ToLower(network))
	if network == "" {
		return ""
	}
	network = strings.ReplaceAll(network, "-", "_")
	network = strings.ReplaceAll(network, " ", "_")
	return network
}

func networkMatches(network, filter string) bool {
	filterNorm := normalizeNetworkName(filter)
	if filterNorm == "" {
		return true
	}
	networkNorm := normalizeNetworkName(network)
	if networkNorm == "" {
		return false
	}
	return networkNorm == filterNorm
}

func networkMatchesAny(network string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, filter := range filters {
		if networkMatches(network, filter) {
			return true
		}
	}
	return false
}
