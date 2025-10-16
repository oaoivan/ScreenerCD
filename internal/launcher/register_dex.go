package launcher

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/gorilla/websocket"
	"github.com/yourusername/screner/internal/assets"
	"github.com/yourusername/screner/internal/config"
	uniswap "github.com/yourusername/screner/internal/dex/Etherium/Uniswap"
	"github.com/yourusername/screner/internal/util"
)

func init() {
	Register("uniswap_v2", buildUniswapV2)
	Register("uniswap_v3", buildUniswapV3)
	Register("uniswap_v4", buildUniswapV4)
}

const (
	launcherDefaultV3PoolABIPath    = "ABI/Uniswap/V3/UniswapV3Pool.json"
	launcherDefaultV4ManagerABIPath = "ABI/Uniswap/V4/UniswapV4PoolManager.json"
)

// buildSupervisorName формирует уникальное имя процесса супервизора с учётом сети и chain id.
func buildSupervisorName(exchange, network string, chainID uint64) string {
	base := strings.ToLower(strings.TrimSpace(exchange))
	if base == "" {
		base = "dex"
	}
	cleanNet := util.NormalizeNetworkName(network, chainID)
	if cleanNet == "" && chainID > 0 {
		cleanNet = fmt.Sprintf("chain-%d", chainID)
	}
	if cleanNet == "" {
		return base
	}
	return fmt.Sprintf("%s:%s", base, cleanNet)
}

func buildUniswapV2(ctx LaunchContext, _ string, _ []string, dexCfg *config.DexConfig) error {
	if ctx.Pricer == nil {
		return fmt.Errorf("uniswap_v2: pricer not configured")
	}
	allowedIdentities := identitySetForDex(ctx.SymbolIdentities, *dexCfg)
	uv2Cfg, err := buildUniswapV2Config(*dexCfg, ctx.Config.ResolveSharedPoolsPath(), ctx.Assets, allowedIdentities)
	if err != nil {
		return err
	}
	dialer := gorillaDialer{}
	connector := uniswap.NewConnector(uv2Cfg, dialer, ctx.Pricer)
	instanceName := buildSupervisorName(uv2Cfg.Exchange, uv2Cfg.Network, uv2Cfg.ChainID)
	util.Infof("uniswap_v2: launch exchange=%s network=%s chain_id=%d pools=%d", uv2Cfg.Exchange, uv2Cfg.Network, uv2Cfg.ChainID, len(uv2Cfg.Pools))
	ctx.Supervisor(instanceName, func() error {
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
	allowedIdentities := identitySetForDex(ctx.SymbolIdentities, *dexCfg)
	v3Cfg, err := buildUniswapV3Config(*dexCfg, ctx.Config.ResolveSharedPoolsPath(), ctx.Assets, allowedIdentities)
	if err != nil {
		return err
	}
	connector := uniswap.NewV3Connector(v3Cfg, ctx.Pricer)
	instanceName := buildSupervisorName(v3Cfg.Exchange, v3Cfg.Network, v3Cfg.ChainID)
	util.Infof("uniswap_v3: launch exchange=%s network=%s chain_id=%d pools_source=%s", v3Cfg.Exchange, v3Cfg.Network, v3Cfg.ChainID, v3Cfg.PoolsPath)
	ctx.Supervisor(instanceName, func() error {
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

func buildUniswapV2Config(dexCfg config.DexConfig, sharedPools string, assetsProvider *assets.Provider, allowedIdentities map[string]struct{}) (uniswap.Config, error) {
	wsURL := dexCfg.WSURL
	if wsURL == "" {
		return uniswap.Config{}, fmt.Errorf("uniswap_v2: ws_url empty")
	}
	httpURL := dexCfg.HTTPURL
	if httpURL == "" {
		return uniswap.Config{}, fmt.Errorf("uniswap_v2: http_url empty")
	}
	path := dexCfg.ResolvePoolsPath(sharedPools)
	registry := uniswap.NewTokenRegistry(assetsProvider, uniswap.RegistryOptions{
		Network:   dexCfg.Network,
		NetworkID: dexCfg.NetworkID,
		ChainID:   dexCfg.ChainIDValue(),
	})
	ps := dexCfg.PoolsSource
	appendUnique := func(list []string, val string) []string {
		trimmed := strings.ToLower(strings.TrimSpace(val))
		if trimmed == "" {
			return list
		}
		for _, existing := range list {
			if existing == trimmed {
				return list
			}
		}
		return append(list, trimmed)
	}
	dexFilters := ps.DexFilters()
	if len(dexFilters) == 0 {
		dexFilters = appendUnique(dexFilters, dexCfg.EffectiveDexAlias())
		dexFilters = appendUnique(dexFilters, dexCfg.Name)
	}
	networkFilters := ps.NetworkFilters()
	if len(networkFilters) == 0 {
		networkFilters = appendUnique(networkFilters, dexCfg.EffectiveNetworkID())
		networkFilters = appendUnique(networkFilters, dexCfg.Network)
		if registry != nil {
			networkFilters = appendUnique(networkFilters, registry.NetworkName())
		}
	}
	wantedFromSource := ps.WantedPairsFor(dexCfg.EffectiveNetworkID())
	wantedPairs := mergeWantedPairs(dexCfg.WantedPairs, wantedFromSource)
	options := uniswap.PoolSourceOptions{
		DexFilters:     dexFilters,
		NetworkFilters: networkFilters,
		WantedPairs:    wantedPairs,
		IncludeStable:  ps.IncludeStable,
		AMMVersions:    ps.AMMFilters(),
	}
	options.IdentityFilters = buildIdentityFiltersForConfig(dexCfg, "v2", dexFilters, networkFilters, options.AMMVersions)
	options.IdentityFilters = filterIdentityFilters(options.IdentityFilters, allowedIdentities)
	pools, err := uniswap.LoadPoolsFromSourceWithOptions(path, registry, options)
	if err != nil {
		return uniswap.Config{}, err
	}
	for i := range pools {
		uniswap.FinalizePool(&pools[i])
	}
	batch := dexCfg.SubscribeBatch
	if batch <= 0 {
		batch = 150
	}
	ping := time.Duration(dexCfg.PingInterval)
	if ping <= 0 {
		ping = 25
	}
	networkID := strings.ToLower(dexCfg.EffectiveNetworkID())
	if networkID == "" {
		networkID = strings.ToLower(strings.TrimSpace(dexCfg.Network))
	}
	exchangeAlias := strings.ToLower(dexCfg.EffectiveDexAlias())
	if exchangeAlias == "" {
		exchangeAlias = strings.ToLower(strings.TrimSpace(dexCfg.Name))
	}
	return uniswap.Config{
		WSURL:              wsURL,
		HTTPURL:            httpURL,
		Exchange:           exchangeAlias,
		Network:            networkID,
		ChainID:            dexCfg.ChainIDValue(),
		Pools:              pools,
		SubscribeBatchSize: batch,
		PingInterval:       ping * time.Second,
	}, nil
}

func buildUniswapV3Config(dexCfg config.DexConfig, sharedPools string, assetsProvider *assets.Provider, allowedIdentities map[string]struct{}) (uniswap.V3Config, error) {
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
	registry := uniswap.NewTokenRegistry(assetsProvider, uniswap.RegistryOptions{
		Network:   dexCfg.Network,
		NetworkID: dexCfg.NetworkID,
		ChainID:   dexCfg.ChainIDValue(),
	})
	appendUnique := func(list []string, val string) []string {
		trimmed := strings.ToLower(strings.TrimSpace(val))
		if trimmed == "" {
			return list
		}
		for _, existing := range list {
			if existing == trimmed {
				return list
			}
		}
		return append(list, trimmed)
	}
	dexFilters := ps.DexFilters()
	if len(dexFilters) == 0 {
		dexFilters = appendUnique(dexFilters, ps.GeckoDex)
		dexFilters = appendUnique(dexFilters, dexCfg.EffectiveDexAlias())
		dexFilters = appendUnique(dexFilters, dexCfg.Name)
	}
	networkFilters := ps.NetworkFilters()
	if len(networkFilters) == 0 {
		networkFilters = appendUnique(networkFilters, ps.GeckoNetwork)
		networkFilters = appendUnique(networkFilters, dexCfg.EffectiveNetworkID())
		networkFilters = appendUnique(networkFilters, dexCfg.Network)
	}
	wantedFromSource := ps.WantedPairsFor(dexCfg.EffectiveNetworkID())
	exchangeName := strings.TrimSpace(dexCfg.DexAlias)
	if exchangeName == "" {
		exchangeName = strings.TrimSpace(dexCfg.Name)
	}
	if exchangeName == "" {
		exchangeName = "uniswap_v3"
	}
	canonicalNetwork := strings.ToLower(dexCfg.EffectiveNetworkID())
	if canonicalNetwork == "" {
		canonicalNetwork = strings.ToLower(strings.TrimSpace(dexCfg.Network))
	}
	abiPath := resolveV3PoolABIPath(dexCfg)
	cfg := uniswap.V3Config{
		Exchange:       strings.ToLower(exchangeName),
		Network:        canonicalNetwork,
		ChainID:        dexCfg.ChainIDValue(),
		WSURL:          wsURL,
		HTTPURL:        httpURL,
		PoolsPath:      path,
		DexFilter:      ps.GeckoDex,
		DexFilters:     dexFilters,
		NetworkFilter:  ps.GeckoNetwork,
		NetworkFilters: networkFilters,
		BatchSize:      dexCfg.SubscribeBatch,
		PingInterval:   time.Duration(dexCfg.PingInterval) * time.Second,
		StopOnAckError: dexCfg.StopOnAckError,
		LogAllEvents:   dexCfg.LogAllEvents,
		DecodeSwapOnly: dexCfg.SwapOnly,
		MaxMetaWorkers: dexCfg.MaxMetaWorkers,
		Registry:       registry,
		WantedPairs:    mergeWantedPairs(dexCfg.WantedPairs, wantedFromSource),
		AMMVersions:    ps.AMMFilters(),
		PoolABIPath:    abiPath,
	}
	cfg.IdentityFilters = buildIdentityFiltersForConfig(dexCfg, "v3", dexFilters, networkFilters, cfg.AMMVersions)
	cfg.IdentityFilters = filterIdentityFilters(cfg.IdentityFilters, allowedIdentities)
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 150
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 25 * time.Second
	}
	if cfg.DexFilter == "" && len(dexFilters) > 0 {
		cfg.DexFilter = dexFilters[0]
	}
	if cfg.NetworkFilter == "" && len(networkFilters) > 0 {
		cfg.NetworkFilter = networkFilters[0]
	}
	util.Infof("uniswap_v3: resolved abi exchange=%s network=%s path=%s", cfg.Exchange, cfg.Network, abiPath)
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

	allowedIdentities := identitySetForDex(ctx.SymbolIdentities, *dexCfg)
	v4Cfg, err := buildUniswapV4Config(*dexCfg, sharedPools, ctx.Assets, allowedIdentities)
	if err != nil {
		return err
	}

	connector, err := uniswap.NewV4Connector(v4Cfg, ctx.Pricer)
	if err != nil {
		return err
	}

	instanceName := buildSupervisorName(v4Cfg.Exchange, v4Cfg.Network, v4Cfg.ChainID)

	util.Infof("uniswap_v4: launch exchange=%s network=%s chain_id=%d inline_pools=%d pools_path=%s once=%v", v4Cfg.Exchange, v4Cfg.Network, v4Cfg.ChainID, len(v4Cfg.Pools), v4Cfg.PoolsPath, v4Cfg.Once)

	ctx.Supervisor(instanceName, func() error {
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

func buildUniswapV4Config(dexCfg config.DexConfig, sharedPools string, assetsProvider *assets.Provider, allowedIdentities map[string]struct{}) (uniswap.V4Config, error) {
	wsURL := strings.TrimSpace(dexCfg.WSURL)
	httpURL := strings.TrimSpace(dexCfg.HTTPURL)
	if wsURL == "" || httpURL == "" {
		return uniswap.V4Config{}, fmt.Errorf("uniswap_v4: ws/http url empty")
	}

	manager := strings.TrimSpace(dexCfg.PoolManager)
	if !common.IsHexAddress(manager) {
		return uniswap.V4Config{}, fmt.Errorf("uniswap_v4: invalid pool_manager")
	}

	registry := uniswap.NewTokenRegistry(assetsProvider, uniswap.RegistryOptions{
		Network:   dexCfg.Network,
		NetworkID: dexCfg.NetworkID,
		ChainID:   dexCfg.ChainIDValue(),
	})
	appendUnique := func(list []string, val string) []string {
		trimmed := strings.ToLower(strings.TrimSpace(val))
		if trimmed == "" {
			return list
		}
		for _, existing := range list {
			if existing == trimmed {
				return list
			}
		}
		return append(list, trimmed)
	}
	dexFilters := dexCfg.PoolsSource.DexFilters()
	if len(dexFilters) == 0 {
		dexFilters = appendUnique(dexFilters, dexCfg.PoolsSource.GeckoDex)
		dexFilters = appendUnique(dexFilters, dexCfg.EffectiveDexAlias())
		dexFilters = appendUnique(dexFilters, dexCfg.Name)
	}
	networkFilters := dexCfg.PoolsSource.NetworkFilters()
	if len(networkFilters) == 0 {
		networkFilters = appendUnique(networkFilters, dexCfg.PoolsSource.GeckoNetwork)
		networkFilters = appendUnique(networkFilters, dexCfg.EffectiveNetworkID())
		networkFilters = appendUnique(networkFilters, dexCfg.Network)
	}
	wantedFromSource := dexCfg.PoolsSource.WantedPairsFor(dexCfg.EffectiveNetworkID())

	inlinePools, err := convertDexPoolsToV4(dexCfg.Pools, registry)
	if err != nil {
		return uniswap.V4Config{}, err
	}

	sharedPath := strings.TrimSpace(sharedPools)
	path := dexCfg.ResolvePoolsPath(sharedPath)

	exchangeName := strings.TrimSpace(dexCfg.DexAlias)
	if exchangeName == "" {
		exchangeName = strings.TrimSpace(dexCfg.Name)
	}
	if exchangeName == "" {
		exchangeName = "uniswap_v4"
	}
	canonicalNetwork := dexCfg.EffectiveNetworkID()
	if canonicalNetwork == "" {
		canonicalNetwork = strings.TrimSpace(dexCfg.Network)
	}
	canonicalNetwork = strings.ToLower(canonicalNetwork)

	managerABI := resolveV4ManagerABIPath(dexCfg)
	cfg := uniswap.V4Config{
		Exchange:           strings.ToLower(exchangeName),
		Network:            canonicalNetwork,
		ChainID:            dexCfg.ChainIDValue(),
		NetworkFilters:     networkFilters,
		DexFilters:         dexFilters,
		AMMVersions:        dexCfg.PoolsSource.AMMFilters(),
		WSURL:              wsURL,
		HTTPURL:            httpURL,
		PoolManager:        common.HexToAddress(manager),
		PoolsPath:          strings.TrimSpace(path),
		Pools:              inlinePools,
		SubscribeBatch:     dexCfg.SubscribeBatch,
		PingInterval:       time.Duration(dexCfg.PingInterval) * time.Second,
		MaxMetaWorkers:     dexCfg.MaxMetaWorkers,
		SwapOnly:           dexCfg.SwapOnly,
		LogAllEvents:       dexCfg.LogAllEvents,
		StopOnAckError:     dexCfg.StopOnAckError,
		Once:               dexCfg.Once,
		WantedPairsOnly:    dexCfg.WantedPairsOnly,
		Registry:           registry,
		PoolManagerABIPath: managerABI,
	}
	cfg.IdentityFilters = buildIdentityFiltersForConfig(dexCfg, "v4", dexFilters, networkFilters, cfg.AMMVersions)
	cfg.IdentityFilters = filterIdentityFilters(cfg.IdentityFilters, allowedIdentities)

	cfg.WantedPairs = mergeWantedPairs(dexCfg.WantedPairs, wantedFromSource)
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

	util.Infof("uniswap_v4: resolved manager abi exchange=%s network=%s path=%s", cfg.Exchange, cfg.Network, managerABI)
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

		dexName := strings.TrimSpace(entry.Dex)
		ammVersion := strings.TrimSpace(entry.AMMVersion)
		network := strings.TrimSpace(entry.Network)

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

		result = append(result, uniswap.V4PoolConfig{
			Dex:           dexName,
			AMMVersion:    ammVersion,
			Network:       network,
			PoolID:        pid,
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

func filterIdentityFilters(filters []string, allowed map[string]struct{}) []string {
	if len(filters) == 0 {
		return filters
	}
	if len(allowed) == 0 {
		return filters
	}
	result := make([]string, 0, len(filters))
	for _, raw := range filters {
		norm := strings.ToLower(strings.TrimSpace(raw))
		if norm == "" {
			continue
		}
		if _, ok := allowed[norm]; ok {
			result = append(result, norm)
		}
	}
	if len(result) == 0 {
		return result
	}
	sort.Strings(result)
	return result
}

func identitySetForDex(identities []util.SymbolIdentity, dexCfg config.DexConfig) map[string]struct{} {
	if len(identities) == 0 {
		return nil
	}
	targetDexes := make(map[string]struct{})
	addDex := func(raw string) {
		variants := expandDexIdentityVariants([]string{raw}, dexCfg)
		for _, variant := range variants {
			if variant == "" {
				continue
			}
			targetDexes[variant] = struct{}{}
		}
	}
	addDex(dexCfg.Name)
	addDex(dexCfg.DexAlias)
	addDex(dexCfg.PoolsSource.GeckoDex)
	for _, pool := range dexCfg.Pools {
		addDex(pool.Dex)
	}
	if len(targetDexes) == 0 {
		return nil
	}
	allowed := make(map[string]struct{})
	for _, id := range identities {
		key := strings.ToLower(id.Key())
		if key == "" {
			continue
		}
		dexNorm := util.NormalizeSymbolDex(id.Dex)
		if dexNorm == "" {
			continue
		}
		if _, ok := targetDexes[dexNorm]; !ok {
			base := util.StripDexVersion(dexNorm)
			if base == "" {
				continue
			}
			if _, ok := targetDexes[base]; !ok {
				continue
			}
		}
		allowed[key] = struct{}{}
	}
	if len(allowed) == 0 {
		return nil
	}
	return allowed
}

// buildIdentityFiltersForConfig собирает комбинации dex+amm+network для фильтрации пулов.
func buildIdentityFiltersForConfig(dexCfg config.DexConfig, fallbackAMM string, dexFilters, networkFilters, ammFilters []string) []string {
	dexes := normalizeForIdentity(dexFilters, normalizeDexForIdentity)
	dexes = appendNormalizedValue(dexes, dexCfg.EffectiveDexAlias(), normalizeDexForIdentity)
	dexes = appendNormalizedValue(dexes, dexCfg.Name, normalizeDexForIdentity)
	dexes = appendNormalizedValue(dexes, dexCfg.PoolsSource.GeckoDex, normalizeDexForIdentity)
	dexes = expandDexIdentityVariants(dexes, dexCfg)

	networks := normalizeForIdentity(networkFilters, normalizeNetworkForIdentity)
	networks = appendNormalizedValue(networks, dexCfg.EffectiveNetworkID(), normalizeNetworkForIdentity)
	networks = appendNormalizedValue(networks, dexCfg.Network, normalizeNetworkForIdentity)
	networks = appendNormalizedValue(networks, dexCfg.PoolsSource.GeckoNetwork, normalizeNetworkForIdentity)

	amms := normalizeForIdentity(ammFilters, normalizeAMMForIdentity)
	amms = appendNormalizedValue(amms, fallbackAMM, normalizeAMMForIdentity)

	if len(dexes) == 0 || len(networks) == 0 || len(amms) == 0 {
		return nil
	}

	set := make(map[string]struct{}, len(dexes)*len(networks)*len(amms))
	for _, dex := range dexes {
		for _, amm := range amms {
			for _, network := range networks {
				if key := composeIdentityKey(dex, amm, network); key != "" {
					set[key] = struct{}{}
				}
			}
		}
	}

	if len(set) == 0 {
		return nil
	}
	result := make([]string, 0, len(set))
	for key := range set {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func normalizeForIdentity(values []string, normalize func(string) string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = appendNormalizedValue(result, value, normalize)
	}
	return result
}

func expandDexIdentityVariants(values []string, dexCfg config.DexConfig) []string {
	if len(values) == 0 {
		return values
	}
	networkNorm := normalizeNetworkForIdentity(dexCfg.EffectiveNetworkID())
	if networkNorm == "" {
		networkNorm = normalizeNetworkForIdentity(dexCfg.Network)
	}
	seen := make(map[string]struct{}, len(values)*3)
	result := make([]string, 0, len(values)*3)
	for _, val := range values {
		variants := dexIdentityVariants(val, networkNorm)
		for _, variant := range variants {
			if variant == "" {
				continue
			}
			if _, ok := seen[variant]; ok {
				continue
			}
			seen[variant] = struct{}{}
			result = append(result, variant)
		}
	}
	return result
}

func dexIdentityVariants(raw string, network string) []string {
	base := normalizeDexForIdentity(raw)
	if base == "" {
		return nil
	}
	variants := []string{base}
	if stripped := util.StripDexVersion(base); stripped != "" {
		variants = append(variants, stripped)
	}
	synonyms := dexSynonymsForNetwork(base, network)
	variants = append(variants, synonyms...)
	return variants
}

func dexSynonymsForNetwork(dex string, network string) []string {
	if dex == "" {
		return nil
	}
	result := make([]string, 0, 4)
	if strings.Contains(dex, "uniswap") {
		result = append(result, "uniswap")
		if strings.Contains(network, "bsc") || strings.Contains(network, "binance") {
			result = append(result, "pancakeswap", "pcs", "pancake")
		}
	}
	if strings.Contains(dex, "pancake") {
		result = append(result, "pancakeswap", "pcs")
	}
	return result
}

func appendNormalizedValue(list []string, raw string, normalize func(string) string) []string {
	normalized := normalize(raw)
	if normalized == "" {
		return list
	}
	for _, existing := range list {
		if existing == normalized {
			return list
		}
	}
	return append(list, normalized)
}

func resolveV3PoolABIPath(dexCfg config.DexConfig) string {
	if path := strings.TrimSpace(dexCfg.PoolABIPath); path != "" {
		if exists(path) {
			return path
		}
		util.Infof("uniswap_v3: configured pool_abi missing path=%s, fallback to default", path)
	}
	alias := util.NormalizeSymbolDex(dexCfg.EffectiveDexAlias())
	switch alias {
	case "pancakeswap", "pancakeswap_v3", "pcs_v3":
		candidate := "ABI/Pancakeswap/V3/PancakeswapV3Pool.json"
		if exists(candidate) {
			return candidate
		}
		util.Infof("uniswap_v3: pancakeswap pool abi not found, reuse uniswap abi")
	}
	return launcherDefaultV3PoolABIPath
}

func resolveV4ManagerABIPath(dexCfg config.DexConfig) string {
	if path := strings.TrimSpace(dexCfg.ManagerABIPath); path != "" {
		if exists(path) {
			return path
		}
		util.Infof("uniswap_v4: configured manager_abi missing path=%s, fallback to default", path)
	}
	alias := util.NormalizeSymbolDex(dexCfg.EffectiveDexAlias())
	switch alias {
	case "pancakeswap_v4", "pancakeswap":
		candidate := "ABI/Pancakeswap/V4/PancakeswapV4PoolManager.json"
		if exists(candidate) {
			return candidate
		}
		util.Infof("uniswap_v4: pancakeswap manager abi not found, reuse uniswap manager abi")
	}
	return launcherDefaultV4ManagerABIPath
}

func exists(path string) bool {
	if path == "" {
		return false
	}
	if _, err := os.Stat(path); err == nil {
		return true
	}
	return false
}

func normalizeDexForIdentity(raw string) string {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return ""
	}
	trimmed = strings.ReplaceAll(trimmed, "-", "_")
	trimmed = strings.ReplaceAll(trimmed, " ", "_")
	return trimmed
}

func normalizeNetworkForIdentity(raw string) string {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return ""
	}
	trimmed = strings.ReplaceAll(trimmed, "-", "_")
	trimmed = strings.ReplaceAll(trimmed, " ", "_")
	return trimmed
}

func normalizeAMMForIdentity(raw string) string {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return ""
	}
	if !strings.HasPrefix(trimmed, "v") {
		trimmed = "v" + trimmed
	}
	return trimmed
}

func composeIdentityKey(dex, amm, network string) string {
	if dex == "" || amm == "" || network == "" {
		return ""
	}
	return fmt.Sprintf("%s|%s|%s", dex, amm, network)
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
