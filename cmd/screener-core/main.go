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

	"github.com/yourusername/screner/internal/config"
	"github.com/yourusername/screner/internal/exchange"
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
	if cfg.DefaultSymbolsFile != "" {
		if list, err := loadSymbolsFromFile(cfg.DefaultSymbolsFile); err != nil {
			util.Errorf("Failed to load default symbols file %s: %v", cfg.DefaultSymbolsFile, err)
		} else {
			defaultBaseSymbols = list
		}
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
			symbolsByExchange[exKey] = baseToQuote(defaultBaseSymbols, cfg.DefaultSymbolsFile)
			util.Infof("Exchange %s uses default symbols (%d base)", exKey, len(defaultBaseSymbols))
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

	// Start connectors per exchange
	for _, ex := range exchanges {
		exLower := strings.ToLower(strings.TrimSpace(ex))
		symbols := symbolsByExchange[exLower]
		switch exLower {
		case "bybit":
			exName := "Bybit"
			url := "wss://stream.bybit.com/v5/public/spot"
			// run supervisor concurrently per exchange
			go supervisor(exName, func() error {
				client := exchange.NewBybitClient(url)
				if err := client.Connect(); err != nil {
					return err
				}
				// subscribe in batches
				batchSize := cfg.SubscribeBatchSize
				if batchSize <= 0 {
					batchSize = 100
				}
				batchPause := time.Duration(cfg.SubscribeBatchPauseMs) * time.Millisecond
				smallDelay := 5 * time.Millisecond
				for i, s := range symbols {
					util.Infof("%s subscribe %d/%d: %s", exName, i+1, len(symbols), s)
					if err := client.Subscribe(s); err != nil {
						util.Errorf("%s subscribe error for %s: %v", exName, s, err)
					}
					// light pacing inside batch
					time.Sleep(smallDelay)
					// pause between batches
					if (i+1)%batchSize == 0 {
						util.Infof("%s subscribed %d symbols, pausing %dms", exName, i+1, cfg.SubscribeBatchPauseMs)
						time.Sleep(batchPause)
					}
				}
				// readers
				go client.ReadLoop(exName, "MULTIPLE_SYMBOLS")
				go client.KeepAlive()
				// forward to shared channel
				for md := range client.Out() {
					select {
					case dataChannel <- md:
					default:
						util.Errorf("dataChannel full, dropping message %s:%s", md.Exchange, md.Symbol)
						atomic.AddInt64(&totalDrops, 1)
					}
				}
				client.Close()
				return fmt.Errorf("%s out channel closed", exName)
			}, stop)

		case "gate", "gateio":
			exName := "Gate"
			url := "wss://api.gateio.ws/ws/v4/"
			// run supervisor concurrently per exchange
			go supervisor(exName, func() error {
				client := exchange.NewGateClient(url)
				if err := client.Connect(); err != nil {
					return err
				}
				// subscribe in batches (convert symbol format)
				batchSize := cfg.SubscribeBatchSize
				if batchSize <= 0 {
					batchSize = 100
				}
				batchPause := time.Duration(cfg.SubscribeBatchPauseMs) * time.Millisecond
				smallDelay := 10 * time.Millisecond
				for i, s := range symbols {
					gateSym := util.BybitToGateSymbol(s)
					util.Debugf("%s subscribe %d/%d: %s -> %s", exName, i+1, len(symbols), s, gateSym)
					if err := client.Subscribe(gateSym); err != nil {
						util.Errorf("%s subscribe error for %s: %v", exName, gateSym, err)
					}
					time.Sleep(smallDelay)
					if (i+1)%batchSize == 0 {
						util.Infof("%s subscribed %d symbols, pausing %dms", exName, i+1, cfg.SubscribeBatchPauseMs)
						time.Sleep(batchPause)
					}
				}
				// readers
				go client.ReadLoop(exName)
				go client.KeepAlive()
				// forward to shared channel
				for md := range client.Out() {
					select {
					case dataChannel <- md:
					default:
						util.Errorf("dataChannel full, dropping message %s:%s", md.Exchange, md.Symbol)
						atomic.AddInt64(&totalDrops, 1)
					}
				}
				client.Close()
				return fmt.Errorf("%s out channel closed", exName)
			}, stop)

		case "bitget":
			exName := "Bitget"
			url := "wss://ws.bitget.com/v2/ws/public"
			go supervisor(exName, func() error {
				client := exchange.NewBitgetClient(url)
				if err := client.Connect(); err != nil {
					return err
				}

				bitgetSymbols := symbols
				util.Infof("%s using symbols list: %d", exName, len(bitgetSymbols))
				// Bitget строгие лимиты: 10 сообщений/сек. Делаем батч-подписку + паузы.
				batchSize := cfg.BitgetSubscribeBatchSize
				if batchSize <= 0 {
					batchSize = 30
				}
				batchPause := time.Duration(cfg.BitgetSubscribePauseMs) * time.Millisecond
				if batchPause <= 0 {
					batchPause = 700 * time.Millisecond
				}
				// формируем батчи instId и шлём одним сообщением
				batch := make([]string, 0, batchSize)
				pushBatch := func(count int) {
					if len(batch) == 0 {
						return
					}
					if err := client.SubscribeBatch(batch); err != nil {
						util.Errorf("%s subscribe batch error (size=%d): %v", exName, len(batch), err)
					}
					util.Infof("%s subscribed %d symbols (batch), pausing %dms", exName, count, cfg.BitgetSubscribePauseMs)
					batch = batch[:0]
					time.Sleep(batchPause)
				}
				total := 0
				for _, s := range bitgetSymbols {
					instId := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(s, "-", ""), "_", ""))
					batch = append(batch, instId)
					total++
					if len(batch) >= batchSize {
						pushBatch(total)
					}
				}
				// остаток
				pushBatch(total)
				go client.ReadLoop(exName)
				// параметризованный ping interval
				pingInterval := time.Duration(cfg.BitgetPingIntervalSec) * time.Second
				go client.KeepAlive(pingInterval)
				for md := range client.Out() {
					select {
					case dataChannel <- md:
					default:
						util.Errorf("dataChannel full, dropping message %s:%s", md.Exchange, md.Symbol)
						atomic.AddInt64(&totalDrops, 1)
					}
				}
				client.Close()
				return fmt.Errorf("%s out channel closed", exName)
			}, stop)
		default:
			// OKX integration
			if exLower == "okx" {
				exName := "OKX"
				url := "wss://ws.okx.com:8443/ws/v5/public"
				go supervisor(exName, func() error {
					client := exchange.NewOkxClient(url)
					if err := client.Connect(); err != nil {
						return err
					}
					// батч-подписка
					batchSize := cfg.SubscribeBatchSize
					if batchSize <= 0 {
						batchSize = 100
					}
					batchPause := time.Duration(cfg.SubscribeBatchPauseMs) * time.Millisecond
					if batchPause <= 0 {
						batchPause = 200 * time.Millisecond
					}
					batch := make([]string, 0, batchSize)
					push := func(count int) {
						if len(batch) == 0 {
							return
						}
						if err := client.SubscribeBatch(batch); err != nil {
							util.Errorf("%s subscribe batch error (size=%d): %v", exName, len(batch), err)
						}
						util.Infof("%s subscribed %d symbols (batch), pausing %dms", exName, count, cfg.SubscribeBatchPauseMs)
						batch = batch[:0]
						time.Sleep(batchPause)
					}
					total := 0
					for _, s := range symbols {
						// OKX instId формат: BASE-QUOTE (например, BTC-USDT)
						instId := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(s, "_", "-"), "/", "-"))
						// если уже в виде BTCUSDT, вставим дефис перед 4-символьным суффиксом
						if !strings.Contains(instId, "-") && len(instId) > 4 {
							instId = instId[:len(instId)-4] + "-" + instId[len(instId)-4:]
						}
						batch = append(batch, instId)
						total++
						if len(batch) >= batchSize {
							push(total)
						}
					}
					push(total)
					go client.ReadLoop(exName)
					pingInterval := time.Duration(cfg.MetricsPeriodSec) * time.Second // используем период метрик как безопасный пинг, либо 30с
					if pingInterval <= 0 {
						pingInterval = 25 * time.Second
					}
					go client.KeepAlive(pingInterval)
					for md := range client.Out() {
						select {
						case dataChannel <- md:
						default:
							util.Errorf("dataChannel full, dropping message %s:%s", md.Exchange, md.Symbol)
							atomic.AddInt64(&totalDrops, 1)
						}
					}
					client.Close()
					return fmt.Errorf("%s out channel closed", exName)
				}, stop)
			} else {
				util.Errorf("unknown exchange in config: %s", ex)
			}
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

	summary := make([]string, 0, len(exchanges))
	for _, ex := range exchanges {
		exKey := strings.ToLower(strings.TrimSpace(ex))
		count := len(symbolsByExchange[exKey])
		summary = append(summary, fmt.Sprintf("%s:%d", exKey, count))
	}
	util.Infof("Screener Core running with exchanges %s", strings.Join(summary, ", "))

	// Redis health checker (updates redisUp flag)
	go func() {
		period := time.Duration(cfg.MetricsPeriodSec) * time.Second
		if period <= 0 {
			period = 5 * time.Second
		}
		ticker := time.NewTicker(period)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := redisClient.Ping(); err != nil {
					atomic.StoreInt32(&redisUp, 0)
				} else {
					atomic.StoreInt32(&redisUp, 1)
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
