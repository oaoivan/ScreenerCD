package config

import (
	"strings"
	"testing"
)

func buildBaseV4Config() DexConfig {
	cfg := DexConfig{
		Name:           "uniswap_v4",
		Network:        "ethereum",
		WSURL:          "wss://example",
		HTTPURL:        "https://example",
		SubscribeBatch: 150,
		PingInterval:   25,
		PoolManager:    "0x0000000000000000000000000000000000000001",
		PoolsFile:      "dummy.json",
		MaxMetaWorkers: 4,
	}
	cfg.applyDefaults()
	return cfg
}

func TestValidateUniswapV4RequiresAssetsRegistry(t *testing.T) {
	cfg := Config{
		DexConfigs: []DexConfig{buildBaseV4Config()},
	}

	sharedAssets := strings.TrimSpace(cfg.AssetsRegistry.Resolve())
	cfg.DexConfigs[0].AssetsPath = cfg.DexConfigs[0].ResolveAssetsPath(sharedAssets)

	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected error when uniswap_v4 configured without assets registry")
	} else if !strings.Contains(err.Error(), "assets") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateUniswapV4WithAssetsRegistry(t *testing.T) {
	cfg := Config{
		AssetsRegistry: FileSource{File: "configs/assets/tokens.yaml"},
		DexConfigs:     []DexConfig{buildBaseV4Config()},
	}

	sharedAssets := strings.TrimSpace(cfg.AssetsRegistry.Resolve())
	cfg.DexConfigs[0].AssetsPath = cfg.DexConfigs[0].ResolveAssetsPath(sharedAssets)

	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}
