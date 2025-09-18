package main

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/yourusername/screner/internal/config"
	"github.com/yourusername/screner/internal/exchange"
	"github.com/yourusername/screner/internal/redisclient"
	"github.com/yourusername/screner/internal/util"
)

func main() {
	util.Infof("starting Screener Core - Step 2: Bybit + Redis integration (Multiple Tickers)")

	// Загружаем конфигурацию для Redis
	cfg, err := config.LoadConfig("configs/screener-core.yaml")
	if err != nil {
		util.Fatalf("Error loading config: %v", err)
	}

	// Инициализируем Redis клиент
	redisClient := redisclient.NewRedisClient(cfg.Redis.RedisAddress(), cfg.Redis.Password, 0)
	util.Infof("Redis client initialized: %s", cfg.Redis.RedisAddress())

	// Загружаем все символы из JSON файла
	symbols, err := util.LoadSymbolsFromFile("../Temp/all_contracts_merged_reformatted.json")
	if err != nil {
		util.Fatalf("Error loading symbols: %v", err)
	}
	util.Infof("Loaded %d symbols from JSON file", len(symbols))

	// Создаем Bybit клиент
	url := "wss://stream.bybit.com/v5/public/spot"
	client := exchange.NewBybitClient(url)

	// Подключаемся к Bybit
	if err := client.Connect(); err != nil {
		util.Fatalf("Failed to connect to Bybit: %v", err)
	}

	util.Infof("Successfully connected to Bybit")

	// Подписываемся на все символы
	var wg sync.WaitGroup
	for _, symbol := range symbols {
		if err := client.Subscribe(symbol); err != nil {
			util.Errorf("Failed to subscribe to %s: %v", symbol, err)
			continue
		}
		util.Debugf("Subscribed to %s", symbol)
		time.Sleep(100 * time.Millisecond) // Небольшая задержка между подписками
	}

	util.Infof("Subscribed to %d symbols", len(symbols))

	// Запускаем ReadLoop в горутине для всех символов
	wg.Add(1)
	go func() {
		defer wg.Done()
		client.ReadLoop("bybit", "multiple")
	}()

	// Запускаем KeepAlive в горутине
	wg.Add(1)
	go func() {
		defer wg.Done()
		client.KeepAlive()
	}()

	// Читаем данные из канала и сохраняем в Redis (согласно шагу 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for marketData := range client.Out() {
			// Выводим данные в консоль для мониторинга
			fmt.Printf("Received MarketData: Exchange=%s, Symbol=%s, Price=%.4f, Timestamp=%d\n",
				marketData.Exchange, marketData.Symbol, marketData.Price, marketData.Timestamp)

			// Сохраняем в Redis с ключом вида price:{exchange}:{symbol}
			redisKey := fmt.Sprintf("price:%s:%s", marketData.Exchange, marketData.Symbol)

			if err := redisClient.HSet(redisKey,
				"price", marketData.Price,
				"timestamp", marketData.Timestamp,
				"exchange", marketData.Exchange,
				"symbol", marketData.Symbol); err != nil {
				util.Errorf("Failed to save to Redis: %v", err)
			} else {
				util.Debugf("Saved to Redis: %s -> price=%.4f", redisKey, marketData.Price)
			}
		}
	}()

	util.Infof("Screener Core is running, monitoring %d symbols and saving to Redis...", len(symbols))

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	util.Infof("Shutting down...")
	client.Close()
	time.Sleep(time.Second)
	wg.Wait()
}
