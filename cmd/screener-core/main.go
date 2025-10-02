package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/yourusername/screner/internal/config"
	"github.com/yourusername/screner/internal/dex/pricing"
	"github.com/yourusername/screner/internal/launcher"
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
	if sharedPoolsPath == "" {
		util.Fatalf("shared pools file is not configured; set shared_pools.file or env")
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

	baseSymbolCache := make(map[string][]string)
	loadSymbolsFromFile := func(path string) ([]string, error) {
		if cached, ok := baseSymbolCache[path]; ok {
			return cached, nil
		}
		list, err := util.LoadSymbolsFromFile(path)
		if err != nil {
			return nil, err
		}
		util.Infof("Loaded %d symbols from %s", len(list), path)
		baseSymbolCache[path] = list
		return list, nil
	}

	var defaultBaseSymbols []string
	defaultSymbolsSource := sharedPoolsPath
	if list, err := loadSymbolsFromFile(sharedPoolsPath); err != nil {
		util.Fatalf("Failed to load shared pools file %s: %v", sharedPoolsPath, err)
	} else {
		defaultBaseSymbols = list
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
			list, err := loadSymbolsFromFile(exCfg.SymbolsFile)
			if err != nil {
				util.Fatalf("Error loading symbols for %s from %s: %v", exKey, exCfg.SymbolsFile, err)
			}
			symbolsByExchange[exKey] = baseToQuote(list, exCfg.SymbolsFile)
		case len(defaultBaseSymbols) > 0:
			symbolsByExchange[exKey] = baseToQuote(defaultBaseSymbols, defaultSymbolsSource)
			util.Infof("Exchange %s uses shared pools source (%d base)", exKey, len(defaultBaseSymbols))
		default:
			list, err := loadSymbolsFromFile(legacySymbolsPath)
			if err != nil {
				util.Fatalf("Error loading legacy symbols for %s: %v", exKey, err)
			}
			symbolsByExchange[exKey] = baseToQuote(list, legacySymbolsPath)
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

	var pricer pricing.Pricer
	if len(cfg.DexConfigs) > 0 {
		anchors := []pricing.TokenInfo{
			{Address: common.HexToAddress("0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"), Symbol: "USDC", Decimals: 6},
			{Address: common.HexToAddress("0xdac17f958d2ee523a2206206994597c13d831ec7"), Symbol: "USDT", Decimals: 6},
			{Address: common.HexToAddress("0x6b175474e89094c44da98b954eedeac495271d0f"), Symbol: "DAI", Decimals: 18},
		}
		pricer = pricing.NewGraphPricer(anchors)
	}

	launchCtx := launcher.LaunchContext{
		Config:      cfg,
		DataChannel: dataChannel,
		Stop:        stop,
		Pricer:      pricer,
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
