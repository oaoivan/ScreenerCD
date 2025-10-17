package util

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadSymbolsFromFile_BasePoolsFilters(t *testing.T) {
	t.Setenv("SYMBOL_LOADER_AMM_FILTER", "v2")
	t.Setenv("SYMBOL_LOADER_INCLUDE_STABLE", "")

	path := writeBasePoolsFixture(t, []basePoolsEntry{
		newBasePoolsEntry("ETH", "v2"),
		newBasePoolsEntry("USDC", "v2"), // stable coin should be skipped by default
		newBasePoolsEntry("ETH", "v2"),  // duplicate
		newBasePoolsEntry("", "v2"),     // empty symbol ignored
		newBasePoolsEntry("WETH", "v3"), // different amm_version filtered out
	})

	got, err := LoadSymbolsFromFile(path)
	if err != nil {
		t.Fatalf("LoadSymbolsFromFile error: %v", err)
	}
	want := []string{"ETH"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("symbols mismatch: got %v want %v", got, want)
	}
}

func TestLoadSymbolsFromFile_BasePoolsIncludeStable(t *testing.T) {
	t.Setenv("SYMBOL_LOADER_AMM_FILTER", "v2")
	t.Setenv("SYMBOL_LOADER_INCLUDE_STABLE", "true")

	path := writeBasePoolsFixture(t, []basePoolsEntry{
		newBasePoolsEntry("ETH", "v2"),
		newBasePoolsEntry("USDC", "v2"),
	})

	got, err := LoadSymbolsFromFile(path)
	if err != nil {
		t.Fatalf("LoadSymbolsFromFile error: %v", err)
	}
	want := []string{"ETH", "USDC"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("symbols mismatch: got %v want %v", got, want)
	}
}

func TestLoadSymbolsFromFile_LegacyMapFallback(t *testing.T) {
	t.Setenv("SYMBOL_LOADER_INCLUDE_STABLE", "")

	tmp := createTempFile(t, "legacy*.json")
	defer tmp.cleanup()

	legacy := map[string]interface{}{
		"btc":  map[string]string{},
		"ETH":  map[string]string{},
		"usdc": map[string]string{}, // should be skipped
		"":     map[string]string{}, // ignored
	}
	enc := json.NewEncoder(tmp.file)
	if err := enc.Encode(legacy); err != nil {
		t.Fatalf("encode legacy: %v", err)
	}

	got, err := LoadSymbolsFromFile(tmp.path)
	if err != nil {
		t.Fatalf("LoadSymbolsFromFile error: %v", err)
	}
	want := []string{"BTC", "ETH"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("symbols mismatch: got %v want %v", got, want)
	}
}

type basePoolsEntry struct {
	Symbol       string `json:"symbol"`
	AMMVersion   string `json:"amm_version"`
	Dex          string `json:"dex"`
	Network      string `json:"network"`
	PoolAddress  string `json:"pool_address"`
	TokenAddress string `json:"token_address"`
	PoolKey      struct {
		Currency0 string `json:"currency0"`
		Currency1 string `json:"currency1"`
	} `json:"pool_key"`
	Token0 basePoolsToken `json:"token0"`
	Token1 basePoolsToken `json:"token1"`
}

type basePoolsToken struct {
	Address  string `json:"address"`
	Symbol   string `json:"symbol"`
	Decimals int    `json:"decimals"`
}

func newBasePoolsEntry(symbol, version string) basePoolsEntry {
	entry := basePoolsEntry{
		Symbol:       symbol,
		AMMVersion:   version,
		Dex:          "uniswap",
		Network:      "ethereum",
		PoolAddress:  "0xb4e16d0168e52d35cacd2c6185b44281ec28c9dc",
		TokenAddress: "0x0000000000000000000000000000000000000000",
	}
	entry.PoolKey.Currency0 = "0x0000000000000000000000000000000000000000"
	entry.PoolKey.Currency1 = "0x0000000000000000000000000000000000000000"
	entry.Token0 = basePoolsToken{
		Address:  "0x0000000000000000000000000000000000000001",
		Symbol:   "TOKEN0",
		Decimals: 18,
	}
	entry.Token1 = basePoolsToken{
		Address:  "0x0000000000000000000000000000000000000002",
		Symbol:   "TOKEN1",
		Decimals: 18,
	}
	return entry
}

func writeBasePoolsFixture(t *testing.T, entries []basePoolsEntry) string {
	t.Helper()
	payload := map[string]interface{}{
		"entries": entries,
	}
	tmp := createTempFile(t, "base_pools*.json")
	defer tmp.file.Close()
	enc := json.NewEncoder(tmp.file)
	if err := enc.Encode(payload); err != nil {
		t.Fatalf("encode base_pools: %v", err)
	}
	return tmp.path
}

type tempFile struct {
	file *os.File
	path string
}

func createTempFile(t *testing.T, pattern string) tempFile {
	t.Helper()
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	return tempFile{file: f, path: filepath.Clean(f.Name())}
}

func (tf tempFile) cleanup() {
	tf.file.Close()
	os.Remove(tf.path)
}
