package processor

import (
	"context"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
	pb "github.com/yourusername/screner/pkg/protobuf"
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

func (p *Processor) ProcessMarketData(marketData *pb.MarketData) {
	key := fmt.Sprintf("price:%s:%s", marketData.Exchange, marketData.Symbol)
	err := p.redisClient.HSet(p.ctx, key, "price", marketData.Price, "timestamp", marketData.Timestamp).Err()
	if err != nil {
		fmt.Printf("Error saving market data to Redis: %v\n", err)
		return
	}
}

func (p *Processor) Start() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			marketData := &pb.MarketData{
				Exchange:  "example_exchange",
				Symbol:    "BTC-USDT",
				Price:     50000.0,
				Timestamp: time.Now().Unix(),
			}
			p.ProcessMarketData(marketData)
		}
	}
}
