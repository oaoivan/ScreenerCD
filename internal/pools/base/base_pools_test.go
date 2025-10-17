package basepools

import (
	"path/filepath"
	"testing"
)

func TestLoadBasePoolsReturnsCopyPerCall(t *testing.T) {
	path := filepath.Join("..", "..", "..", "testdata", "base_pools_samples.json")

	first, err := LoadBasePools(path)
	if err != nil {
		t.Fatalf("LoadBasePools first call: %v", err)
	}
	if len(first) == 0 {
		t.Fatalf("LoadBasePools returned no entries for %s", path)
	}

	originalSymbol := first[0].Symbol
	first[0].Symbol = "MUTATED"

	second, err := LoadBasePools(path)
	if err != nil {
		t.Fatalf("LoadBasePools second call: %v", err)
	}
	if len(second) == 0 {
		t.Fatalf("LoadBasePools second call returned no entries")
	}
	if second[0].Symbol != originalSymbol {
		t.Fatalf("cached entry mutated: got %s want %s", second[0].Symbol, originalSymbol)
	}
}

func TestFilterEntriesByDexVersionAndNetwork(t *testing.T) {
	path := filepath.Join("..", "..", "..", "testdata", "base_pools_samples.json")
	entries, err := LoadBasePools(path)
	if err != nil {
		t.Fatalf("LoadBasePools: %v", err)
	}

	filter := Filter{
		Dexes:    []string{"Uniswap_V3"},
		Networks: []string{"ethereum"},
		Versions: []Version{VersionV3},
	}

	filtered := FilterEntries(entries, filter)
	if len(filtered) == 0 {
		t.Fatalf("FilterEntries returned no entries for %+v", filter)
	}

	for _, entry := range filtered {
		if NormalizeDex(entry.Dex) != "uniswap" {
			t.Fatalf("unexpected dex: %s", entry.Dex)
		}
		if entry.Network != "ethereum" {
			t.Fatalf("unexpected network: %s", entry.Network)
		}
		if v, _ := ParseAMMVersion(entry.AMMVersion); v != VersionV3 {
			t.Fatalf("unexpected version: %s", entry.AMMVersion)
		}
	}
}

func TestNormalizeDexVariants(t *testing.T) {
	cases := map[string]string{
		"Uniswap":        "uniswap",
		"uniswap_v3":     "uniswap",
		"pancake_v2":     "pancake",
		"pancakeswap_v2": "pancakeswap",
		"  sushiswap  ":  "sushiswap",
	}

	for input, want := range cases {
		if got := NormalizeDex(input); got != want {
			t.Errorf("NormalizeDex(%q) = %q, want %q", input, got, want)
		}
	}
}
