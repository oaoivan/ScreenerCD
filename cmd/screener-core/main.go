package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
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

	// Shared buffered channel (increase buffer to handle bursts)
	dataChannel := make(chan *pb.MarketData, 8192)

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
				// subscribe all
				for i, s := range symbols {
					util.Infof("%s subscribe %d/%d: %s", exName, i+1, len(symbols), s)
					if err := client.Subscribe(s); err != nil {
						util.Errorf("%s subscribe error for %s: %v", exName, s, err)
					}
					time.Sleep(10 * time.Millisecond)
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
				// subscribe all (convert symbol format)
				for i, s := range symbols {
					gateSym := util.BybitToGateSymbol(s)
					util.Infof("%s subscribe %d/%d: %s -> %s", exName, i+1, len(symbols), s, gateSym)
					if err := client.Subscribe(gateSym); err != nil {
						util.Errorf("%s subscribe error for %s: %v", exName, gateSym, err)
					}
					time.Sleep(15 * time.Millisecond)
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
					}
				}
				client.Close()
				return fmt.Errorf("%s out channel closed", exName)
			}, stop)
		default:
			util.Errorf("unknown exchange in config: %s", ex)
		}
	}

	// Consumers: small worker pool to save to Redis concurrently
	numWorkers := 8
	for i := 0; i < numWorkers; i++ {
		go func(workerID int) {
			for marketData := range dataChannel {
				key := fmt.Sprintf("price:%s:%s", marketData.Exchange, marketData.Symbol)
				if err := redisClient.HSet(key,
					"price", marketData.Price,
					"timestamp", marketData.Timestamp,
					"exchange", marketData.Exchange,
					"symbol", marketData.Symbol); err != nil {
					util.Errorf("Worker %d: Failed to save to Redis: %v", workerID, err)
				} else {
					util.Debugf("Worker %d: Saved to Redis: %s -> price=%.6f ts=%d", workerID, key, marketData.Price, marketData.Timestamp)
				}
			}
		}(i)
	}

	util.Infof("Screener Core running: %d symbols across %s", len(symbols), strings.Join(exchanges, ", "))

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	util.Infof("Shutting down...")
	close(stop)
	// allow some time for goroutines to exit
	time.Sleep(2 * time.Second)
}
