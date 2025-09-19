package main

import (
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

	// Load symbols (Bybit format, e.g., BTCUSDT)
	symbols, err := util.LoadSymbolsFromFile("Temp/all_contracts_merged_reformatted.json")
	if err != nil {
		util.Fatalf("Error loading symbols: %v", err)
	}
	util.Infof("Loaded %d symbols from JSON file", len(symbols))

	// Shared buffered channel (configurable)
	dataChannel := make(chan *pb.MarketData, cfg.DataChannelBuffer)

	// Stop channel (close on signal)
	stop := make(chan struct{})

	// Build list of exchanges
	exchanges := cfg.Exchanges
	if len(exchanges) == 0 && cfg.Exchange != "" {
		exchanges = []string{cfg.Exchange}
	}
	if len(exchanges) == 0 {
		exchanges = []string{"bybit"}
	}
	util.Infof("Exchanges: %s", strings.Join(exchanges, ", "))

	// Metrics
	var totalProcessed int64
	var totalRedisOps int64
	var totalDrops int64
	var mu sync.Mutex
	perExchange := map[string]int64{}

	// Start connectors per exchange
	for _, ex := range exchanges {
		exLower := strings.ToLower(strings.TrimSpace(ex))
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
		default:
			util.Errorf("unknown exchange in config: %s", ex)
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
					util.Errorf("Worker %d: pipeline exec error: %v", workerID, err)
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
					key := fmt.Sprintf("price:%s:%s", md.Exchange, md.Symbol)
					entry := []interface{}{key, "price", md.Price, "timestamp", md.Timestamp, "exchange", md.Exchange, "symbol", md.Symbol}
					batch = append(batch, entry)
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

	util.Infof("Screener Core running: %d symbols across %s", len(symbols), strings.Join(exchanges, ", "))

	// Periodic metrics logger
	go func() {
		period := time.Duration(cfg.MetricsPeriodSec) * time.Second
		if period <= 0 {
			period = 5 * time.Second
		}
		var prevProcessed, prevRedisOps, prevDrops int64
		for range time.Tick(period) {
			curProcessed := atomic.LoadInt64(&totalProcessed)
			curRedisOps := atomic.LoadInt64(&totalRedisOps)
			curDrops := atomic.LoadInt64(&totalDrops)
			dMsgs := curProcessed - prevProcessed
			dOps := curRedisOps - prevRedisOps
			dDrops := curDrops - prevDrops
			prevProcessed, prevRedisOps, prevDrops = curProcessed, curRedisOps, curDrops
			mu.Lock()
			// snapshot map
			perExSnapshot := make(map[string]int64, len(perExchange))
			for k, v := range perExchange {
				perExSnapshot[k] = v
			}
			mu.Unlock()
			util.Infof("metrics: msgs/s~%d, redisOps/s~%d, drops/s~%d, chanLen=%d, perExchange=%v", int64(float64(dMsgs)/period.Seconds()+0.5), int64(float64(dOps)/period.Seconds()+0.5), int64(float64(dDrops)/period.Seconds()+0.5), len(dataChannel), perExSnapshot)
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
