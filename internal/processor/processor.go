package processor

import (
	"context"
	"fmt"
	"time"

	"github.com/yourusername/screner/internal/redisclient"
	"github.com/yourusername/screner/internal/util"
	pb "github.com/yourusername/screner/pkg/protobuf"
)

// Processor отвечает за приём рыночных данных и запись агрегатов в Redis.
type Processor struct {
	redis *redisclient.RedisClient
	ctx   context.Context
}

// NewProcessor создаёт обработчик с заданным Redis клиентом.
func NewProcessor(redisClient *redisclient.RedisClient) *Processor {
	ctx := context.Background()
	return &Processor{redis: redisClient, ctx: ctx}
}

// ProcessMarketData сохраняет рыночные данные в Redis.
func (p *Processor) ProcessMarketData(marketData *pb.MarketData) {
	if p == nil || p.redis == nil || marketData == nil {
		return
	}
	networkSegment := util.NormalizeNetworkName(marketData.Network, uint64(marketData.ChainID))
	dexSegment := util.NormalizeMarketDex(marketData.Dex, marketData.Exchange)
	if dexSegment == "" {
		dexSegment = util.NormalizeMarketDex(marketData.Exchange, marketData.Exchange)
	}
	ammSegment := util.NormalizeMarketAMM(marketData.AMMVersion, dexSegment)
	if ammSegment == "" {
		ammSegment = util.DefaultAMMForDex(dexSegment)
	}
	identityKey := fmt.Sprintf("%s|%s|%s", dexSegment, ammSegment, networkSegment)
	marketData.Dex = dexSegment
	marketData.AMMVersion = ammSegment
	marketData.Network = networkSegment
	key := fmt.Sprintf("price:%s:%s:%s:%s", networkSegment, dexSegment, ammSegment, marketData.Symbol)
	if err := p.redis.HSet(key,
		"price", marketData.Price,
		"timestamp", marketData.Timestamp,
		"exchange", marketData.Exchange,
		"symbol", marketData.Symbol,
		"network", networkSegment,
		"chain_id", marketData.ChainID,
		"dex", dexSegment,
		"amm_version", ammSegment,
		"pool_identity", identityKey,
	); err != nil {
		util.Errorf("processor: redis hset key=%s exchange=%s dex=%s amm=%s network=%s chain=%d err=%v", key, marketData.Exchange, dexSegment, ammSegment, networkSegment, marketData.ChainID, err)
	}
}

// Start демонстрирует циклическую обработку данных (пока заглушка для интеграции).
func (p *Processor) Start() {
	if p == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			data := &pb.MarketData{
				Exchange:   "example_exchange",
				Symbol:     "BTC-USDT",
				Price:      50000.0,
				Timestamp:  time.Now().Unix(),
				Network:    util.NormalizeNetworkName("example_network", 0),
				ChainID:    0,
				Dex:        util.NormalizeMarketDex("example_exchange", "example_exchange"),
				AMMVersion: util.DefaultAMMForDex(util.NormalizeMarketDex("example_exchange", "example_exchange")),
			}
			p.ProcessMarketData(data)
		}
	}
}
