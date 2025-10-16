package util

import "strings"

// NormalizeMarketDex возвращает нормализованный алиас DEX/биржи для redis-ключей.
func NormalizeMarketDex(rawDex, fallback string) string {
	if norm := NormalizeSymbolDex(rawDex); norm != "" {
		return norm
	}
	return NormalizeSymbolDex(fallback)
}

// NormalizeMarketAMM приводит amm_version к виду vX или spot, используя алиас площадки.
func NormalizeMarketAMM(rawAMM, dexAlias string) string {
	if norm := NormalizeSymbolAMM(rawAMM); norm != "" {
		return norm
	}
	if derived := DeriveAMMFromAlias(dexAlias); derived != "" {
		return derived
	}
	return DefaultAMMForDex(dexAlias)
}

// DeriveAMMFromAlias пытается извлечь версию AMM из алиаса вида dex_v2.
func DeriveAMMFromAlias(alias string) string {
	norm := NormalizeSymbolDex(alias)
	if norm == "" {
		return ""
	}
	idx := strings.LastIndex(norm, "_v")
	if idx <= 0 || idx == len(norm)-2 {
		return ""
	}
	suffix := norm[idx+1:]
	return NormalizeSymbolAMM(suffix)
}

// DefaultAMMForDex возвращает стандартный тип площадки, когда явной версии нет.
func DefaultAMMForDex(dexAlias string) string {
	norm := NormalizeSymbolDex(dexAlias)
	if norm == "" {
		return "spot"
	}
	if strings.Contains(norm, "perp") || strings.Contains(norm, "futures") {
		return "perp"
	}
	return "spot"
}

// ComposeIdentity нормализует тройку dex/amm/network для redis-ключей.
func ComposeIdentity(dex, amm, network string, chainID uint64, fallback string) SymbolIdentity {
	return SymbolIdentity{
		Dex:        NormalizeMarketDex(dex, fallback),
		AMMVersion: NormalizeMarketAMM(amm, dex),
		Network:    NormalizeNetworkName(network, chainID),
	}
}
