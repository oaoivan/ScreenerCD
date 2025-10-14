package main

import (
	"fmt"
	"log"
	"os"

	"github.com/yourusername/screner/internal/config"
)

func main() {
	// set fallbacks to satisfy validation when env vars absent
	if os.Getenv("POOLMANAGER_V4") == "" {
		os.Setenv("POOLMANAGER_V4", "0x0000000000000000000000000000000000000001")
	}
	if os.Getenv("POOLMANAGER_V4_BSC") == "" {
		os.Setenv("POOLMANAGER_V4_BSC", "0x0000000000000000000000000000000000000002")
	}

	cfg, err := config.LoadConfig("configs/screener-core.yaml")
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}
	sharedPools := cfg.ResolveSharedPoolsPath()
	fmt.Printf("dex_configs=%d\n", len(cfg.DexConfigs))
	for i := range cfg.DexConfigs {
		dex := cfg.DexConfigs[i]
		poolsPath := dex.ResolvePoolsPath(sharedPools)
		fmt.Printf("%02d name=%s network=%s alias=%s ws=%s pools=%s amm_filters=%v\n",
			i, dex.Name, dex.EffectiveNetworkID(), dex.EffectiveDexAlias(), dex.WSURL, poolsPath, dex.PoolsSource.AMMFilters())
	}
}
