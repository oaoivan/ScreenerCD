package uniswap

import (
	"fmt"
	"strings"
)

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
	if dexNorm == filterNorm {
		return true
	}
	return stripDexVersion(dexNorm) != "" && stripDexVersion(dexNorm) == stripDexVersion(filterNorm)
}

func matchesAMMVersion(ammVersion, dex, want string) bool {
	return matchesAnyAMMVersion(ammVersion, []string{want})
}

func matchesAnyAMMVersion(ammVersion string, wants []string) bool {
	norm := normalizeAMMVersion(ammVersion)
	if len(wants) == 0 {
		return true
	}
	if norm == "" {
		return false
	}
	for _, want := range wants {
		if normalizeAMMVersion(want) == norm {
			return true
		}
	}
	return false
}

func normalizeAMMVersion(version string) string {
	trimmed := strings.ToLower(strings.TrimSpace(version))
	if trimmed == "" {
		return ""
	}
	if !strings.HasPrefix(trimmed, "v") {
		trimmed = "v" + trimmed
	}
	return trimmed
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

func identityKey(dex, ammVersion, network string) string {
	dexNorm := normalizeDexName(dex)
	ammNorm := normalizeAMMVersion(ammVersion)
	networkNorm := normalizeNetworkName(network)
	if dexNorm == "" || ammNorm == "" || networkNorm == "" {
		return ""
	}
	return fmt.Sprintf("%s|%s|%s", dexNorm, ammNorm, networkNorm)
}

func stripDexVersion(raw string) string {
	if raw == "" {
		return ""
	}
	idx := strings.Index(raw, "_v")
	if idx <= 0 {
		return raw
	}
	return strings.TrimSuffix(raw[:idx], "_")
}
