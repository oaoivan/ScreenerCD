package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yourusername/screner/internal/config"
	"github.com/yourusername/screner/internal/exchange"
	"github.com/yourusername/screner/internal/redisclient"
	"github.com/yourusername/screner/internal/util"
)

func main() {
	fmt.Println("=== ТЕСТИРОВАНИЕ ВСЕХ ТИКЕРОВ НА BYBIT ===")
	
	// Загружаем конфигурацию
	cfg, err := config.LoadConfig("configs/screener-core.yaml")
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}
	
	// Инициализируем Redis клиент
	redisClient := redisclient.NewRedisClient(cfg.Redis.RedisAddress(), cfg.Redis.Password, 0)
	fmt.Printf("✅ Redis client initialized: %s\n", cfg.Redis.RedisAddress())
	
	// Загружаем все символы из JSON файла
	symbols, err := util.LoadSymbolsFromFile("Temp/all_contracts_merged_reformatted.json")
	if err != nil {
		log.Fatalf("Error loading symbols: %v", err)
	}
	fmt.Printf("✅ Loaded %d symbols from JSON file\n", len(symbols))
	
	// Показываем первые 10 символов для проверки
	fmt.Println("📋 Первые 10 символов:")
	for i, symbol := range symbols {
		if i >= 10 {
			break
		}
		fmt.Printf("   %d. %s\n", i+1, symbol)
	}
	
	// Создаем Bybit клиент
	url := "wss://stream.bybit.com/v5/public/spot"
	client := exchange.NewBybitClient(url)
	
	// Подключаемся к Bybit
	if err := client.Connect(); err != nil {
		log.Fatalf("Failed to connect to Bybit: %v", err)
	}
	fmt.Println("✅ Successfully connected to Bybit")
	
	// Подписываемся на первые 5 символов для теста
	testSymbols := symbols[:5]
	for _, symbol := range testSymbols {
		if err := client.Subscribe(symbol); err != nil {
			fmt.Printf("❌ Failed to subscribe to %s: %v\n", symbol, err)
			continue
		}
		fmt.Printf("✅ Subscribed to %s\n", symbol)
		time.Sleep(500 * time.Millisecond)
	}
	
	// Запускаем ReadLoop в горутине
	go client.ReadLoop("bybit", "test")
	
	// Запускаем KeepAlive в горутине
	go client.KeepAlive()
	
	// Читаем данные и сохраняем в Redis
	receivedTickers := make(map[string]int)
	go func() {
		for marketData := range client.Out() {
			receivedTickers[marketData.Symbol]++
			
			// Выводим данные в консоль
			fmt.Printf("📊 %s: %.4f (получено %d раз)\n", 
				marketData.Symbol, marketData.Price, receivedTickers[marketData.Symbol])
			
			// Сохраняем в Redis
			redisKey := fmt.Sprintf("price:%s:%s", marketData.Exchange, marketData.Symbol)
			if err := redisClient.HSet(redisKey, 
				"price", marketData.Price,
				"timestamp", marketData.Timestamp,
				"exchange", marketData.Exchange,
				"symbol", marketData.Symbol); err != nil {
				fmt.Printf("❌ Failed to save %s to Redis: %v\n", marketData.Symbol, err)
			}
		}
	}()
	
	fmt.Printf("🚀 Monitoring %d test symbols...\n", len(testSymbols))
	fmt.Println("📈 Ожидание данных по тикерам...")
	
	// Ждем 30 секунд и показываем статистику
	time.Sleep(30 * time.Second)
	
	fmt.Println("\n📊 СТАТИСТИКА ЗА 30 СЕКУНД:")
	for symbol, count := range receivedTickers {
		fmt.Printf("   %s: %d сообщений\n", symbol, count)
	}
	
	// Graceful shutdown
	fmt.Println("\n🔄 Завершение тестирования...")
	client.Close()
	
	fmt.Println("✅ ТЕСТИРОВАНИЕ ЗАВЕРШЕНО")
}