package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/yourusername/screner/internal/assets"
	"github.com/yourusername/screner/internal/config"
	"github.com/yourusername/screner/internal/dex/pricing"
	"github.com/yourusername/screner/internal/launcher"
	basepools "github.com/yourusername/screner/internal/pools/base"
	"github.com/yourusername/screner/internal/redisclient"
	"github.com/yourusername/screner/internal/util"
	pb "github.com/yourusername/screner/pkg/protobuf"
)

// supervisor runs fn with panic recovery and restart backoff
func supervisor(name string, fn func() error, stop <-chan struct{}) {
	backoff := time.Second
	for {
		done := make(chan struct{})
		go func() {
			defer func() {
				if r := recover(); r != nil {
					util.Errorf("%s panic: %v", name, r)
				}
				close(done)
			}()
			if err := fn(); err != nil {
				util.Errorf("%s exited with error: %v", name, err)
			} else {
				util.Errorf("%s exited without error", name)
			}
		}()

		select {
		case <-stop:
			util.Infof("%s stop requested", name)
			return
		case <-done:
			// restart with backoff
		}

		util.Infof("restarting %s after %s", name, backoff)
		select {
		case <-stop:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func main() {
	util.Infof("starting Screener Core - multi-exchange with shared channel")

	// Load config
	cfg, err := config.LoadConfig("configs/screener-core.yaml")
	if err != nil {
		util.Fatalf("Error loading config: %v", err)
	}
	sharedPoolsPath := strings.TrimSpace(cfg.ResolveSharedPoolsPath())
	var assetProvider *assets.Provider
	if sharedPoolsPath == "" {
		util.Fatalf("shared pools file is not configured; set shared_pools.file or env")
	}

	assetsPath := strings.TrimSpace(cfg.ResolveAssetsRegistryPath())
	if assetsPath != "" {
		if provider, err := assets.LoadOrGetProvider(assetsPath); err != nil {
			util.Errorf("failed to load assets registry %s: %v", assetsPath, err)
			assetProvider = nil
		} else {
			assetProvider = provider
			util.Infof("Assets registry loaded: %s", assetsPath)
		}
	} else if len(cfg.DexConfigs) > 0 {
		util.Infof("Assets registry path not configured; using fallback stable anchors")
	}

	// Init Redis
	redisClient := redisclient.NewRedisClient(cfg.Redis.RedisAddress(), cfg.Redis.Password, 0)
	util.Infof("Redis client initialized: %s", cfg.Redis.RedisAddress())

	// Build list of exchanges (preserve legacy fallbacks)
	exchanges := cfg.Exchanges
	if len(exchanges) == 0 && cfg.Exchange != "" {
		exchanges = []string{cfg.Exchange}
	}
	if len(exchanges) == 0 {
		exchanges = []string{"bybit"}
	}
	util.Infof("Exchanges from config: %s", strings.Join(exchanges, ", "))

	// Resolve symbols per exchange
	exConfigByName := make(map[string]config.ExchangeConfig)
	for _, exCfg := range cfg.ExchangeConfigs {
		name := strings.ToLower(strings.TrimSpace(exCfg.Name))
		if name == "" {
			continue
		}
		exConfigByName[name] = exCfg
	}

	type symbolCacheKey struct {
		Path          string
		Dex           string
		Network       string
		Version       string
		SymbolsOnly   bool
		IncludeStable bool
	}
	symbolCache := make(map[symbolCacheKey][]string)
	loadSymbols := func(ps config.PoolsSource, explicitPath string) ([]string, string, error) {
		path := strings.TrimSpace(explicitPath)
		if path == "" {
			path = strings.TrimSpace(ps.Resolve())
		}
		if path == "" {
			return nil, "", fmt.Errorf("pools source path is empty")
		}

		key := symbolCacheKey{
			Path:          path,
			Dex:           strings.ToLower(strings.TrimSpace(ps.DexFilter)),
			Network:       strings.ToLower(strings.TrimSpace(ps.NetworkFilter)),
			Version:       strings.ToLower(strings.TrimSpace(ps.AmmVersion)),
			SymbolsOnly:   ps.SymbolsOnly,
			IncludeStable: ps.IncludeStable,
		}
		if cached, ok := symbolCache[key]; ok {
			return append([]string(nil), cached...), path, nil
		}

		useBase := ps.SymbolsOnly || key.Dex != "" || key.Network != "" || key.Version != ""
		if !useBase {
			baseName := strings.ToLower(filepath.Base(path))
			if strings.Contains(baseName, "base_pools") {
				useBase = true
			}
		}

		var symbols []string
		if useBase {
			entries, err := basepools.LoadBasePools(path)
			if err != nil {
				return nil, "", err
			}
			filter := basepools.Filter{}
			if trimmed := strings.TrimSpace(ps.DexFilter); trimmed != "" {
				filter.Dexes = []string{trimmed}
			}
			if trimmed := strings.TrimSpace(ps.NetworkFilter); trimmed != "" {
				filter.Networks = []string{trimmed}
			}
			if trimmed := strings.TrimSpace(ps.AmmVersion); trimmed != "" {
				if parsed, err := basepools.ParseAMMVersion(trimmed); err == nil {
					filter.Versions = []basepools.Version{parsed}
				} else {
					util.Errorf("invalid amm_version %q for %s: %v", ps.AmmVersion, path, err)
				}
			}
			filtered := basepools.FilterEntries(entries, filter)
			if len(filtered) == 0 {
				return nil, "", fmt.Errorf("base pools filter produced zero entries for %s", path)
			}
			skipStable := !ps.IncludeStable
			symbols = basepools.ExtractSymbols(filtered, skipStable)
			if len(symbols) == 0 {
				return nil, "", fmt.Errorf("no symbols extracted from %s after filtering", path)
			}
			util.Infof("Loaded %d symbols from %s (dex=%s network=%s amm=%s symbols_only=%v)", len(symbols), path, ps.DexFilter, ps.NetworkFilter, ps.AmmVersion, ps.SymbolsOnly)
		} else {
			list, err := util.LoadSymbolsFromFile(path)
			if err != nil {
				return nil, "", err
			}
			symbols = list
			util.Infof("Loaded %d symbols from %s", len(symbols), path)
		}

		symbolCache[key] = append([]string(nil), symbols...)
		return append([]string(nil), symbols...), path, nil
	}

	var defaultBaseSymbols []string
	defaultSymbolsSource := ""
	if list, source, err := loadSymbols(cfg.SharedPools, sharedPoolsPath); err != nil {
		util.Fatalf("Failed to load shared pools file %s: %v", sharedPoolsPath, err)
	} else {
		defaultBaseSymbols = list
		defaultSymbolsSource = source
	}

	const legacySymbolsPath = "Temp/all_contracts_merged_reformatted.json"
	symbolsByExchange := make(map[string][]string)
	for _, ex := range exchanges {
		exKey := strings.ToLower(strings.TrimSpace(ex))
		if exKey == "" {
			continue
		}
		exCfg, ok := exConfigByName[exKey]
		baseToQuote := func(bases []string, source string) []string {
			pairs := util.AttachQuote(bases, "USDT")
			if len(pairs) == 0 {
				util.Fatalf("No symbols produced for %s from %s", exKey, source)
			}
			util.Infof("Exchange %s prepared %d symbols (quote=USDT) from %s", exKey, len(pairs), source)
			return pairs
		}

		switch {
		case ok && len(exCfg.Symbols) > 0:
			symbolsByExchange[exKey] = append([]string(nil), exCfg.Symbols...)
			util.Infof("Exchange %s uses %d inline symbols from config", exKey, len(exCfg.Symbols))
		case ok && exCfg.SymbolsFile != "":
			list, source, err := loadSymbols(config.PoolsSource{}, exCfg.SymbolsFile)
			if err != nil {
				util.Fatalf("Error loading symbols for %s from %s: %v", exKey, exCfg.SymbolsFile, err)
			}
			symbolsByExchange[exKey] = baseToQuote(list, source)
		case len(defaultBaseSymbols) > 0:
			symbolsByExchange[exKey] = baseToQuote(defaultBaseSymbols, defaultSymbolsSource)
			util.Infof("Exchange %s uses shared pools source (%d base)", exKey, len(defaultBaseSymbols))
		default:
			list, source, err := loadSymbols(config.PoolsSource{}, legacySymbolsPath)
			if err != nil {
				util.Fatalf("Error loading legacy symbols for %s: %v", exKey, err)
			}
			symbolsByExchange[exKey] = baseToQuote(list, source)
		}
		if len(symbolsByExchange[exKey]) == 0 {
			util.Fatalf("No symbols resolved for exchange %s", exKey)
		}
	}

	// Shared buffered channel (configurable)
	dataChannel := make(chan *pb.MarketData, cfg.DataChannelBuffer)

	// Stop channel (close on signal)
	stop := make(chan struct{})

	// Metrics
	var totalProcessed int64
	var totalRedisOps int64
	var totalRedisErrors int64
	var totalDrops int64
	var redisUp int32 // 1 = up, 0 = down
	var mu sync.Mutex
	perExchange := map[string]int64{}

	pricerRequiredBy := make([]string, 0, len(cfg.DexConfigs))
	seenPricerDex := make(map[string]struct{}, len(cfg.DexConfigs))
	for i := range cfg.DexConfigs {
		trimmed := strings.TrimSpace(cfg.DexConfigs[i].Name)
		if trimmed == "" {
			continue
		}
		nameLower := strings.ToLower(trimmed)
		switch nameLower {
		case "uniswap_v2", "uniswap_v3", "uniswap_v4":
			if _, ok := seenPricerDex[nameLower]; !ok {
				pricerRequiredBy = append(pricerRequiredBy, trimmed)
				seenPricerDex[nameLower] = struct{}{}
			}
		}
	}

	var pricer pricing.Pricer
	if len(pricerRequiredBy) > 0 {
		fallbackAnchors := []pricing.TokenInfo{
			{Address: common.HexToAddress("0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"), Symbol: "USDC", Decimals: 6},
			{Address: common.HexToAddress("0xdac17f958d2ee523a2206206994597c13d831ec7"), Symbol: "USDT", Decimals: 6},
			{Address: common.HexToAddress("0x6b175474e89094c44da98b954eedeac495271d0f"), Symbol: "DAI", Decimals: 18},
		}
		pricer = pricing.NewGraphPricerFromAssets(assetProvider, nil, fallbackAnchors)
		util.Infof("Graph pricer enabled for DEX connectors: %s", strings.Join(pricerRequiredBy, ", "))
	} else {
		util.Infof("Graph pricer not required: no matching DEX connectors")
	}

	launchCtx := launcher.LaunchContext{
		Config:      cfg,
		DataChannel: dataChannel,
		Stop:        stop,
		Pricer:      pricer,
		Assets:      assetProvider,
		Supervisor: func(name string, fn func() error) {
			go supervisor(name, fn, stop)
		},
	}

	for _, ex := range exchanges {
		exLower := strings.ToLower(strings.TrimSpace(ex))
		builder := launcher.Get(exLower)
		if builder == nil {
			util.Errorf("unknown exchange in config: %s", ex)
			continue
		}
		symbols := symbolsByExchange[exLower]
		if err := builder(launchCtx, ex, symbols, nil); err != nil {
			util.Errorf("launch %s error: %v", ex, err)
		}
	}

	for i := range cfg.DexConfigs {
		dexCfg := &cfg.DexConfigs[i]
		name := strings.ToLower(strings.TrimSpace(dexCfg.Name))
		if name == "" {
			continue
		}
		builder := launcher.Get(name)
		if builder == nil {
			util.Errorf("dex %s not supported yet", dexCfg.Name)
			continue
		}
		if err := builder(launchCtx, dexCfg.Name, nil, dexCfg); err != nil {
			util.Errorf("launch %s error: %v", dexCfg.Name, err)
		}
	}

	// Consumers: worker pool with Redis pipelining
	numWorkers := cfg.RedisWorkers
	if numWorkers <= 0 {
		numWorkers = 8
	}
	pipelineSize := cfg.RedisPipelineSize
	if pipelineSize <= 0 {
		pipelineSize = 300
	}

	for i := 0; i < numWorkers; i++ {
		go func(workerID int) {
			batch := make([][]interface{}, 0, pipelineSize)
			timer := time.NewTimer(100 * time.Millisecond)
			defer timer.Stop()
			flush := func() {
				if len(batch) == 0 {
					return
				}
				if err := redisClient.HSetBatch(batch); err != nil {
					// агрегируем ошибки пайплайна без спама
					atomic.AddInt64(&totalRedisErrors, 1)
					util.Debugf("Worker %d: pipeline exec error: %v", workerID, err)
				} else {
					atomic.AddInt64(&totalRedisOps, int64(len(batch)))
				}
				batch = batch[:0]
			}
			for {
				select {
				case md, ok := <-dataChannel:
					if !ok {
						flush()
						return
					}
					// Raw key (как было)
					keyRaw := fmt.Sprintf("price:%s:%s", md.Exchange, md.Symbol)
					entryRaw := []interface{}{keyRaw, "price", md.Price, "timestamp", md.Timestamp, "exchange", md.Exchange, "symbol", md.Symbol}
					batch = append(batch, entryRaw)

					// Canonical key для арбитража (нормализуем спот-символ)
					canon := util.NormalizeSpotSymbol(md.Exchange, md.Symbol)
					keyCanon := fmt.Sprintf("price_canon:%s:%s", canon, md.Exchange)
					entryCanon := []interface{}{keyCanon, "price", md.Price, "timestamp", md.Timestamp, "exchange", md.Exchange, "symbol", md.Symbol}
					batch = append(batch, entryCanon)
					// metrics: processed messages
					atomic.AddInt64(&totalProcessed, 1)
					mu.Lock()
					perExchange[md.Exchange]++
					mu.Unlock()
					if len(batch) >= pipelineSize {
						flush()
						if !timer.Stop() {
							select {
							case <-timer.C:
							default:
							}
						}
						timer.Reset(100 * time.Millisecond)
					}
				case <-timer.C:
					flush()
					timer.Reset(100 * time.Millisecond)
				}
			}
		}(i)
	}

	// Redis health monitor
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := redisClient.Ping(); err != nil {
					atomic.StoreInt32(&redisUp, 0)
					util.Errorf("redis ping failed: %v", err)
				} else {
					atomic.StoreInt32(&redisUp, 1)
					util.Debugf("redis ping ok")
				}
			case <-stop:
				return
			}
		}
	}()

	// Periodic metrics logger
	go func() {
		period := time.Duration(cfg.MetricsPeriodSec) * time.Second
		if period <= 0 {
			period = 5 * time.Second
		}
		var prevProcessed, prevRedisOps, prevDrops, prevRedisErrors int64
		for range time.Tick(period) {
			curProcessed := atomic.LoadInt64(&totalProcessed)
			curRedisOps := atomic.LoadInt64(&totalRedisOps)
			curRedisErrors := atomic.LoadInt64(&totalRedisErrors)
			curDrops := atomic.LoadInt64(&totalDrops)
			dMsgs := curProcessed - prevProcessed
			dOps := curRedisOps - prevRedisOps
			dErrs := curRedisErrors - prevRedisErrors
			dDrops := curDrops - prevDrops
			prevProcessed, prevRedisOps, prevRedisErrors, prevDrops = curProcessed, curRedisOps, curRedisErrors, curDrops
			mu.Lock()
			// snapshot map
			perExSnapshot := make(map[string]int64, len(perExchange))
			for k, v := range perExchange {
				perExSnapshot[k] = v
			}
			mu.Unlock()
			up := atomic.LoadInt32(&redisUp) == 1
			util.Infof("metrics: msgs/s~%d, redisOps/s~%d, redisErr/s~%d, redisUp=%t, drops/s~%d, chanLen=%d, perExchange=%v",
				int64(float64(dMsgs)/period.Seconds()+0.5),
				int64(float64(dOps)/period.Seconds()+0.5),
				int64(float64(dErrs)/period.Seconds()+0.5),
				up,
				int64(float64(dDrops)/period.Seconds()+0.5),
				len(dataChannel), perExSnapshot)
		}
	}()

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	util.Infof("Shutting down...")
	close(stop)
	// allow some time for goroutines to exit
	time.Sleep(2 * time.Second)
}

// loadLinesFile reads non-empty, trimmed lines from a text file; returns empty slice on error.
func loadLinesFile(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		util.Errorf("loadLinesFile open error: %v", err)
		return nil
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	res := make([]string, 0, 512)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		res = append(res, line)
	}
	if err := s.Err(); err != nil {
		util.Errorf("loadLinesFile scan error: %v", err)
	}
	return res
}
