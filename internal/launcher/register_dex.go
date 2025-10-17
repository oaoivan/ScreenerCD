package launcher

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/gorilla/websocket"
	"github.com/yourusername/screner/internal/assets"
	"github.com/yourusername/screner/internal/config"
	uniswap "github.com/yourusername/screner/internal/dex/Etherium/Uniswap"
	basepools "github.com/yourusername/screner/internal/pools/base"
	"github.com/yourusername/screner/internal/util"
)

func init() {
	Register("uniswap_v2", buildUniswapV2)
	Register("uniswap_v3", buildUniswapV3)
	Register("uniswap_v4", buildUniswapV4)
}

func buildUniswapV2(ctx LaunchContext, _ string, _ []string, dexCfg *config.DexConfig) error {
	if ctx.Pricer == nil {
		return fmt.Errorf("uniswap_v2: pricer not configured")
	}
	uv2Cfg, err := buildUniswapV2Config(*dexCfg, ctx.Config.ResolveSharedPoolsPath(), ctx.Assets)
	if err != nil {
		return err
	}
	dialer := gorillaDialer{}
	connector := uniswap.NewConnector(uv2Cfg, dialer, ctx.Pricer)
	name := uv2Cfg.Exchange
	if name == "" {
		name = "uniswap_v2"
	}
	ctx.Supervisor(name, func() error {
		cCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-ctx.Stop:
				cancel()
			case <-cCtx.Done():
			}
		}()
		return connector.Run(cCtx, ctx.DataChannel)
	})
	return nil
}

func buildUniswapV3(ctx LaunchContext, _ string, _ []string, dexCfg *config.DexConfig) error {
	if ctx.Pricer == nil {
		return fmt.Errorf("uniswap_v3: pricer not configured")
	}
	v3Cfg, err := buildUniswapV3Config(*dexCfg, ctx.Config.ResolveSharedPoolsPath(), ctx.Assets)
	if err != nil {
		return err
	}
	connector := uniswap.NewV3Connector(v3Cfg, ctx.Pricer)
	name := v3Cfg.Exchange
	if name == "" {
		name = "uniswap_v3"
	}
	ctx.Supervisor(name, func() error {
		cCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-ctx.Stop:
				cancel()
			case <-cCtx.Done():
			}
		}()
		return connector.Run(cCtx, ctx.DataChannel)
	})
	return nil
}

func buildUniswapV2Config(dexCfg config.DexConfig, sharedPools string, assetsProvider *assets.Provider) (uniswap.Config, error) {
	wsURL := dexCfg.WSURL
	if wsURL == "" {
		return uniswap.Config{}, fmt.Errorf("uniswap_v2: ws_url empty")
	}
	httpURL := dexCfg.HTTPURL
	if httpURL == "" {
		return uniswap.Config{}, fmt.Errorf("uniswap_v2: http_url empty")
	}
	path := dexCfg.ResolvePoolsPath(sharedPools)
	registry := uniswap.NewTokenRegistry(assetsProvider, dexCfg.Network)
	dexFilter := strings.TrimSpace(dexCfg.PoolsSource.DexFilter)
	if dexFilter == "" {
		dexFilter = strings.TrimSpace(dexCfg.Name)
	}
	dexFilter = basepools.NormalizeDex(dexFilter)

	filter := basepools.Filter{}
	if dexFilter != "" {
		filter.Dexes = []string{dexFilter}
	}
	if net := strings.TrimSpace(dexCfg.PoolsSource.NetworkFilter); net != "" {
		filter.Networks = []string{net}
	} else if net := strings.TrimSpace(dexCfg.Network); net != "" {
		filter.Networks = []string{net}
	}
	ammFilter := strings.TrimSpace(dexCfg.PoolsSource.AmmVersionFilter)
	if ammFilter == "" {
		ammFilter = strings.TrimSpace(dexCfg.PoolsSource.AmmVersion)
	}
	if ammFilter != "" {
		if v, err := basepools.ParseAMMVersion(ammFilter); err == nil && v != basepools.VersionUnknown {
			filter.Versions = []basepools.Version{v}
		}
	}
	pools, err := uniswap.LoadPoolsFromBaseWithRegistry(path, registry, filter)
	if err != nil {
		return uniswap.Config{}, err
	}
	for i := range pools {
		uniswap.FinalizePool(&pools[i])
	}
	util.Infof("uniswap_v2: pools source=%s filters dex=%v networks=%v versions=%v total=%d", path, filter.Dexes, filter.Networks, filter.Versions, len(pools))
	batch := dexCfg.SubscribeBatch
	if batch <= 0 {
		batch = 150
	}
	ping := time.Duration(dexCfg.PingInterval)
	if ping <= 0 {
		ping = 25
	}
	return uniswap.Config{
		WSURL:              wsURL,
		HTTPURL:            httpURL,
		Exchange:           dexCfg.Name,
		Pools:              pools,
		SubscribeBatchSize: batch,
		PingInterval:       ping * time.Second,
	}, nil
}

func buildUniswapV3Config(dexCfg config.DexConfig, sharedPools string, assetsProvider *assets.Provider) (uniswap.V3Config, error) {
	wsURL := dexCfg.WSURL
	httpURL := dexCfg.HTTPURL
	if wsURL == "" || httpURL == "" {
		return uniswap.V3Config{}, fmt.Errorf("uniswap_v3: ws/http url empty")
	}
	path := dexCfg.ResolvePoolsPath(sharedPools)
	if path == "" {
		return uniswap.V3Config{}, fmt.Errorf("uniswap_v3: pools path empty")
	}
	ps := dexCfg.PoolsSource
	registry := uniswap.NewTokenRegistry(assetsProvider, dexCfg.Network)
	ammFilter := strings.TrimSpace(ps.AmmVersionFilter)
	if ammFilter == "" {
		ammFilter = strings.TrimSpace(ps.AmmVersion)
	}
	cfg := uniswap.V3Config{
		Exchange:       dexCfg.Name,
		WSURL:          wsURL,
		HTTPURL:        httpURL,
		PoolsPath:      path,
		DexFilter:      ps.DexFilter,
		NetworkFilter:  ps.NetworkFilter,
		AmmVersion:     ammFilter,
		BatchSize:      dexCfg.SubscribeBatch,
		PingInterval:   time.Duration(dexCfg.PingInterval) * time.Second,
		StopOnAckError: dexCfg.StopOnAckError,
		LogAllEvents:   dexCfg.LogAllEvents,
		DecodeSwapOnly: dexCfg.SwapOnly,
		MaxMetaWorkers: dexCfg.MaxMetaWorkers,
		Registry:       registry,
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 150
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 25 * time.Second
	}
	util.Infof("uniswap_v3 config: pools_path=%s dex_filter=%s network_filter=%s amm_filter=%s batch=%d", cfg.PoolsPath, strings.TrimSpace(cfg.DexFilter), strings.TrimSpace(cfg.NetworkFilter), strings.TrimSpace(cfg.AmmVersion), cfg.BatchSize)
	return cfg, nil
}

type gorillaDialer struct{}

func buildUniswapV4(ctx LaunchContext, _ string, _ []string, dexCfg *config.DexConfig) error {
	if ctx.Pricer == nil {
		return fmt.Errorf("uniswap_v4: pricer not configured")
	}
	if dexCfg == nil {
		return fmt.Errorf("uniswap_v4: missing configuration")
	}

	var sharedPools string
	if ctx.Config != nil {
		sharedPools = ctx.Config.ResolveSharedPoolsPath()
	}

	v4Cfg, err := buildUniswapV4Config(*dexCfg, sharedPools, ctx.Assets)
	if err != nil {
		return err
	}

	connector, err := uniswap.NewV4Connector(v4Cfg, ctx.Pricer)
	if err != nil {
		return err
	}

	name := v4Cfg.Exchange
	if name == "" {
		name = "uniswap_v4"
	}

	util.Infof("uniswap_v4: launch inline_pools=%d pools_path=%s once=%v", len(v4Cfg.Pools), v4Cfg.PoolsPath, v4Cfg.Once)

	ctx.Supervisor(name, func() error {
		cCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-ctx.Stop:
				cancel()
			case <-cCtx.Done():
			}
		}()
		return connector.Run(cCtx, ctx.DataChannel)
	})

	return nil
}

func (gorillaDialer) Dial(ctx context.Context, endpoint string) (uniswap.WSConnection, error) {
	d := *websocket.DefaultDialer
	d.HandshakeTimeout = 15 * time.Second
	conn, _, err := d.DialContext(ctx, endpoint, nil)
	if err != nil {
		return nil, err
	}
	return &gorillaConn{conn: conn}, nil
}

func buildUniswapV4Config(dexCfg config.DexConfig, sharedPools string, assetsProvider *assets.Provider) (uniswap.V4Config, error) {
	wsURL := strings.TrimSpace(dexCfg.WSURL)
	httpURL := strings.TrimSpace(dexCfg.HTTPURL)
	if wsURL == "" || httpURL == "" {
		return uniswap.V4Config{}, fmt.Errorf("uniswap_v4: ws/http url empty")
	}

	manager := strings.TrimSpace(dexCfg.PoolManager)
	if !common.IsHexAddress(manager) {
		return uniswap.V4Config{}, fmt.Errorf("uniswap_v4: invalid pool_manager")
	}

	registry := uniswap.NewTokenRegistry(assetsProvider, dexCfg.Network)

	inlinePools, err := convertDexPoolsToV4(dexCfg.Pools, registry)
	if err != nil {
		return uniswap.V4Config{}, err
	}

	sharedPath := strings.TrimSpace(sharedPools)
	path := dexCfg.ResolvePoolsPath(sharedPath)

	exchangeName := strings.TrimSpace(dexCfg.Name)
	if exchangeName == "" {
		exchangeName = "uniswap_v4"
	}

	cfg := uniswap.V4Config{
		Exchange:        strings.ToLower(exchangeName),
		Network:         strings.ToLower(strings.TrimSpace(dexCfg.Network)),
		WSURL:           wsURL,
		HTTPURL:         httpURL,
		PoolManager:     common.HexToAddress(manager),
		PoolsPath:       strings.TrimSpace(path),
		Pools:           inlinePools,
		SubscribeBatch:  dexCfg.SubscribeBatch,
		PingInterval:    time.Duration(dexCfg.PingInterval) * time.Second,
		MaxMetaWorkers:  dexCfg.MaxMetaWorkers,
		SwapOnly:        dexCfg.SwapOnly,
		LogAllEvents:    dexCfg.LogAllEvents,
		StopOnAckError:  dexCfg.StopOnAckError,
		Once:            dexCfg.Once,
		WantedPairsOnly: dexCfg.WantedPairsOnly,
		Registry:        registry,
	}

	cfg.WantedPairs = mergeWantedPairs(dexCfg.WantedPairs, dexCfg.PoolsSource.WantedPairs)
	if cfg.SubscribeBatch <= 0 {
		cfg.SubscribeBatch = 150
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 25 * time.Second
	}
	if cfg.MaxMetaWorkers <= 0 {
		cfg.MaxMetaWorkers = 4
	}
	if cfg.Exchange == "" {
		cfg.Exchange = "uniswap_v4"
	}
	util.Infof("uniswap_v4 config: inline_pools=%d pools_path=%s wanted_only=%v", len(inlinePools), cfg.PoolsPath, cfg.WantedPairsOnly)

	return cfg, nil
}

func convertDexPoolsToV4(pools []config.DexPoolConfig, registry *uniswap.TokenRegistry) ([]uniswap.V4PoolConfig, error) {
	if len(pools) == 0 {
		return nil, nil
	}
	result := make([]uniswap.V4PoolConfig, 0, len(pools))
	for idx := range pools {
		entry := pools[idx]
		poolID := strings.TrimSpace(entry.Address)
		if poolID == "" {
			return nil, fmt.Errorf("uniswap_v4: pool %d missing pool_id", idx)
		}
		if len(poolID) != 66 || !strings.HasPrefix(strings.ToLower(poolID), "0x") {
			return nil, fmt.Errorf("uniswap_v4: pool %d invalid pool_id %s", idx, poolID)
		}
		pid := common.HexToHash(poolID)

		token0, err := buildTokenMetaForV4(registry, entry.Token0Address, entry.Token0Symbol, int(entry.Token0Decimals))
		if err != nil {
			return nil, fmt.Errorf("uniswap_v4: pool %d token0: %w", idx, err)
		}
		token1, err := buildTokenMetaForV4(registry, entry.Token1Address, entry.Token1Symbol, int(entry.Token1Decimals))
		if err != nil {
			return nil, fmt.Errorf("uniswap_v4: pool %d token1: %w", idx, err)
		}

		pairName := strings.TrimSpace(entry.PairName)
		if pairName == "" {
			pairName = fmt.Sprintf("%s/%s", token0.Symbol, token1.Symbol)
		}

		poolAddr, err := parseOptionalHexAddress(entry.PoolAddress)
		if err != nil {
			return nil, fmt.Errorf("uniswap_v4: pool %d pool_address: %w", idx, err)
		}
		hookAddr, err := parseOptionalHexAddress(entry.HookAddress)
		if err != nil {
			return nil, fmt.Errorf("uniswap_v4: pool %d hook_address: %w", idx, err)
		}
		tickSpacing := entry.TickSpacing
		if tickSpacing < 0 {
			return nil, fmt.Errorf("uniswap_v4: pool %d invalid tick_spacing=%d", idx, tickSpacing)
		}

		result = append(result, uniswap.V4PoolConfig{
			PoolID:        pid,
			PoolAddress:   poolAddr,
			HookAddress:   hookAddr,
			TickSpacing:   tickSpacing,
			PairName:      pairName,
			Token0:        token0,
			Token1:        token1,
			BaseIsToken0:  entry.BaseIsToken0,
			CanonicalPair: strings.TrimSpace(entry.CanonicalPair),
		})
	}
	return result, nil
}

func buildTokenMetaForV4(registry *uniswap.TokenRegistry, addrStr, symbol string, decimals int) (uniswap.TokenMeta, error) {
	trimmed := strings.TrimSpace(addrStr)
	if !common.IsHexAddress(trimmed) {
		return uniswap.TokenMeta{}, fmt.Errorf("invalid address %s", addrStr)
	}
	addr := common.HexToAddress(trimmed)
	meta := registry.Resolve(addr, symbol, decimals)
	if meta.Address == (common.Address{}) {
		meta.Address = addr
		meta.Symbol = strings.ToUpper(strings.TrimSpace(symbol))
		if meta.Symbol == "" {
			meta.Symbol = strings.ToUpper(strings.TrimPrefix(addr.Hex(), "0x"))
		}
		meta.Decimals = decimals
		if meta.Decimals <= 0 {
			meta.Decimals = 18
		}
	}
	return meta, nil
}

func parseOptionalHexAddress(raw string) (common.Address, error) {
	addr := strings.TrimSpace(raw)
	if addr == "" {
		return common.Address{}, nil
	}
	lower := strings.ToLower(addr)
	if strings.HasPrefix(lower, "0x") && len(lower) == 66 {
		lower = "0x" + lower[len(lower)-40:]
	}
	if !common.IsHexAddress(lower) {
		return common.Address{}, fmt.Errorf("invalid address %s", raw)
	}
	return common.HexToAddress(lower), nil
}

func mergeWantedPairs(primary, secondary []string) []string {
	if len(primary) == 0 && len(secondary) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(primary)+len(secondary))
	result := make([]string, 0, len(primary)+len(secondary))
	add := func(list []string) {
		for _, raw := range list {
			trimmed := strings.TrimSpace(raw)
			if trimmed == "" {
				continue
			}
			key := strings.ToUpper(trimmed)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, trimmed)
		}
	}
	add(primary)
	add(secondary)
	return result
}

type gorillaConn struct {
	conn *websocket.Conn
}

func (g *gorillaConn) WriteJSON(v interface{}) error {
	return g.conn.WriteJSON(v)
}

func (g *gorillaConn) ReadMessage() (int, []byte, error) {
	return g.conn.ReadMessage()
}

func (g *gorillaConn) WriteMessage(messageType int, data []byte) error {
	return g.conn.WriteMessage(messageType, data)
}

func (g *gorillaConn) Close() error {
	return g.conn.Close()
}
