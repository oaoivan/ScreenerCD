package launcher

import (
	"fmt"
	"strings"
	"time"

	"github.com/yourusername/screner/internal/config"
	"github.com/yourusername/screner/internal/exchange"
	"github.com/yourusername/screner/internal/util"
	pb "github.com/yourusername/screner/pkg/protobuf"
)

func init() {
	Register("bybit", buildBybit)
	Register("gate", buildGate)
	Register("gateio", buildGate)
	Register("bitget", buildBitget)
	Register("okx", buildOkx)
}

func buildBybit(ctx LaunchContext, exName string, symbols []string, _ *config.DexConfig) error {
	if len(symbols) == 0 {
		return fmt.Errorf("bybit: empty symbols")
	}
	url := "wss://stream.bybit.com/v5/public/spot"
	ctx.Supervisor("Bybit", func() error {
		client := exchange.NewBybitClient(url)
		if err := client.Connect(); err != nil {
			return err
		}
		batchSize := ctx.Config.SubscribeBatchSize
		if batchSize <= 0 {
			batchSize = 100
		}
		smallDelay := 5 * time.Millisecond
		batchPause := time.Duration(ctx.Config.SubscribeBatchPauseMs) * time.Millisecond
		for i, s := range symbols {
			util.Infof("Bybit subscribe %d/%d: %s", i+1, len(symbols), s)
			_ = client.Subscribe(s)
			time.Sleep(smallDelay)
			if (i+1)%batchSize == 0 {
				time.Sleep(batchPause)
			}
		}
		go client.ReadLoop("Bybit", "MULTIPLE_SYMBOLS")
		go client.KeepAlive()
		forward(ctx, client.Out())
		client.Close()
		return fmt.Errorf("Bybit out channel closed")
	})
	return nil
}

func buildGate(ctx LaunchContext, exName string, symbols []string, _ *config.DexConfig) error {
	if len(symbols) == 0 {
		return fmt.Errorf("gate: empty symbols")
	}
	name := "Gate"
	url := "wss://api.gateio.ws/ws/v4/"
	ctx.Supervisor(name, func() error {
		client := exchange.NewGateClient(url)
		if err := client.Connect(); err != nil {
			return err
		}
		batchSize := ctx.Config.SubscribeBatchSize
		if batchSize <= 0 {
			batchSize = 100
		}
		smallDelay := 10 * time.Millisecond
		batchPause := time.Duration(ctx.Config.SubscribeBatchPauseMs) * time.Millisecond
		for i, s := range symbols {
			gateSym := util.BybitToGateSymbol(s)
			if err := client.Subscribe(gateSym); err != nil {
				util.Errorf("Gate subscribe error for %s: %v", gateSym, err)
			}
			time.Sleep(smallDelay)
			if (i+1)%batchSize == 0 {
				time.Sleep(batchPause)
			}
		}
		go client.ReadLoop(name)
		go client.KeepAlive()
		forward(ctx, client.Out())
		client.Close()
		return fmt.Errorf("%s out channel closed", name)
	})
	return nil
}

func buildBitget(ctx LaunchContext, exName string, symbols []string, _ *config.DexConfig) error {
	if len(symbols) == 0 {
		return fmt.Errorf("bitget: empty symbols")
	}
	url := "wss://ws.bitget.com/v2/ws/public"
	ctx.Supervisor("Bitget", func() error {
		client := exchange.NewBitgetClient(url)
		if err := client.Connect(); err != nil {
			return err
		}
		batchSize := ctx.Config.BitgetSubscribeBatchSize
		if batchSize <= 0 {
			batchSize = 30
		}
		batchPause := time.Duration(ctx.Config.BitgetSubscribePauseMs) * time.Millisecond
		if batchPause <= 0 {
			batchPause = 700 * time.Millisecond
		}
		batch := make([]string, 0, batchSize)
		push := func() {
			if len(batch) == 0 {
				return
			}
			_ = client.SubscribeBatch(batch)
			batch = batch[:0]
			time.Sleep(batchPause)
		}
		for _, s := range symbols {
			instId := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(s, "-", ""), "_", ""))
			batch = append(batch, instId)
			if len(batch) >= batchSize {
				push()
			}
		}
		push()
		pingInterval := time.Duration(ctx.Config.BitgetPingIntervalSec) * time.Second
		if pingInterval <= 0 {
			pingInterval = 25 * time.Second
		}
		go client.ReadLoop("Bitget")
		go client.KeepAlive(pingInterval)
		forward(ctx, client.Out())
		client.Close()
		return fmt.Errorf("Bitget out channel closed")
	})
	return nil
}

func buildOkx(ctx LaunchContext, exName string, symbols []string, _ *config.DexConfig) error {
	if len(symbols) == 0 {
		return fmt.Errorf("okx: empty symbols")
	}
	url := "wss://ws.okx.com:8443/ws/v5/public"
	ctx.Supervisor("OKX", func() error {
		client := exchange.NewOkxClient(url)
		if err := client.Connect(); err != nil {
			return err
		}
		batchSize := ctx.Config.SubscribeBatchSize
		if batchSize <= 0 {
			batchSize = 100
		}
		batchPause := time.Duration(ctx.Config.SubscribeBatchPauseMs) * time.Millisecond
		if batchPause <= 0 {
			batchPause = 200 * time.Millisecond
		}
		batch := make([]string, 0, batchSize)
		flush := func() {
			if len(batch) == 0 {
				return
			}
			_ = client.SubscribeBatch(batch)
			batch = batch[:0]
			time.Sleep(batchPause)
		}
		for _, s := range symbols {
			instId := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(s, "_", "-"), "/", "-"))
			if !strings.Contains(instId, "-") && len(instId) > 4 {
				instId = instId[:len(instId)-4] + "-" + instId[len(instId)-4:]
			}
			batch = append(batch, instId)
			if len(batch) >= batchSize {
				flush()
			}
		}
		flush()
		go client.ReadLoop("OKX")
		pingInterval := time.Duration(ctx.Config.MetricsPeriodSec) * time.Second
		if pingInterval <= 0 {
			pingInterval = 25 * time.Second
		}
		go client.KeepAlive(pingInterval)
		forward(ctx, client.Out())
		client.Close()
		return fmt.Errorf("OKX out channel closed")
	})
	return nil
}

func forward(ctx LaunchContext, ch <-chan *pb.MarketData) {
	for {
		select {
		case <-ctx.Stop:
			return
		case md, ok := <-ch:
			if !ok {
				return
			}
			ctx.DataChannel <- md
		}
	}
}
