package processor

import (
    "context"
    "fmt"
    "time"

    "github.com/go-redis/redis/v8"
    "your_project/internal/redisclient"
    "your_project/pkg/protobuf"
)

type Processor struct {
    redisClient *redis.Client
    ctx         context.Context
}

func NewProcessor(redisClient *redis.Client) *Processor {
    return &Processor{
        redisClient: redisClient,
        ctx:         context.Background(),
    }
}

func (p *Processor) ProcessMarketData(marketData *protobuf.MarketData) {
    // Save market data to Redis
    key := fmt.Sprintf("price:%s:%s", marketData.Exchange, marketData.Symbol)
    err := p.redisClient.HSet(p.ctx, key, "price", marketData.Price, "timestamp", marketData.Timestamp).Err()
    if err != nil {
        fmt.Printf("Error saving market data to Redis: %v\n", err)
        return
    }

    // Logic to calculate arbitrage opportunities can be added here
    // For example, compare prices from different exchanges and log opportunities
}

func (p *Processor) Start() {
    // This method can be used to start listening for market data from exchanges
    // and process it accordingly
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            // Simulate receiving market data
            // In a real application, this would come from a channel or WebSocket
            marketData := &protobuf.MarketData{
                Exchange: "example_exchange",
                Symbol:   "BTC-USDT",
                Price:    50000.0,
                Timestamp: time.Now().Unix(),
            }
            p.ProcessMarketData(marketData)
        }
    }
}