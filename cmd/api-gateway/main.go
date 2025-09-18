package main

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yourusername/screner/internal/config"
	"github.com/yourusername/screner/internal/exchange"
	"github.com/yourusername/screner/internal/redisclient"
	"github.com/yourusername/screner/internal/util"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	util.Infof("starting API Gateway")

	// Load configuration
	cfg, err := config.LoadConfig("configs/screener-core.yaml")
	if err != nil {
		util.Fatalf("Error loading config: %v", err)
	}

	util.SetLevel("debug")

	// Initialize Redis client with error handling
	redisAddr := cfg.Redis.RedisAddress()
	util.Infof("Attempting to connect to Redis at %s", redisAddr)
	redisClient := redisclient.NewRedisClient(redisAddr, cfg.Redis.Password, 0)
	defer func() {
		if redisClient != nil {
			redisClient.Close()
		}
	}()

	// Test Redis connection
	if err := redisClient.Ping(); err != nil {
		util.Errorf("Redis connection failed: %v - continuing without Redis", err)
		redisClient = nil // disable Redis operations
	} else {
		util.Infof("Redis client successfully connected")
	}

	// Load symbols from JSON file
	symbols, err := util.LoadSymbolsFromFile("Temp/all_contracts_merged_reformatted.json")
	if err != nil {
		util.Fatalf("Failed to load symbols: %v", err)
	}
	util.Infof("Loaded %d symbols from JSON file", len(symbols))

	// Create and start Bybit client (mainnet v5 public spot)
	bybitURL := "wss://stream.bybit.com/v5/public/spot"

	bybit := exchange.NewBybitClient(bybitURL)
	if err := bybit.Connect(); err != nil {
		util.Fatalf("Failed to connect to Bybit: %v", err)
	}

	// Subscribe to all symbols
	var wg sync.WaitGroup
	for i, symbol := range symbols {
		util.Infof("Subscribing to symbol %d/%d: %s", i+1, len(symbols), symbol)
		if err := bybit.Subscribe(symbol); err != nil {
			util.Errorf("Bybit subscribe error for %s: %v", symbol, err)
		}
		// Small delay to avoid rate limiting
		time.Sleep(10 * time.Millisecond)
	}

	// Start read loop in goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		bybit.ReadLoop("Bybit", "MULTIPLE_SYMBOLS")
	}()

	util.Infof("Listening for market data from %d symbols...", len(symbols))

	// Consume market data and save to Redis
	go func() {
		for md := range bybit.Out() {
			key := fmt.Sprintf("price:%s:%s", md.Exchange, md.Symbol)
			// Save to Redis if available
			if redisClient != nil {
				if err := redisClient.HSet(key, "price", md.Price, "timestamp", md.Timestamp); err != nil {
					util.Errorf("Error saving market data to Redis: %v", err)
				} else {
					util.Infof("Market Data saved to Redis: %s -> price=%f timestamp=%d", key, md.Price, md.Timestamp)
				}
			} else {
				util.Infof("Market Data (Redis disabled): %s -> price=%f timestamp=%d", key, md.Price, md.Timestamp)
			}
		}
	}()

	// Set up WebSocket route for web interface
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			util.Errorf("Error upgrading connection: %v", err)
			return
		}
		defer conn.Close()

		// Handle WebSocket connection
		for {
			// Read message from WebSocket
			_, msg, err := conn.ReadMessage()
			if err != nil {
				util.Errorf("Error reading message: %v", err)
				break
			}

			// Process the message (currently just log it)
			util.Debugf("/ws received message: %s", string(msg))
		}
	})

	// Start the server
	util.Infof("API Gateway is running on :8080")

	// Wait for all goroutines (this will run indefinitely)
	wg.Wait()
	util.Infof("All connections closed, exiting")
}
