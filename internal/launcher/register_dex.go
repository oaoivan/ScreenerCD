package launcher

import (
	"context"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yourusername/screner/internal/assets"
	"github.com/yourusername/screner/internal/config"
	uniswap "github.com/yourusername/screner/internal/dex/Etherium/Uniswap"
)

func init() {
	Register("uniswap_v2", buildUniswapV2)
	Register("uniswap_v3", buildUniswapV3)
}

func buildUniswapV2(ctx LaunchContext, _ string, _ []string, dexCfg *config.DexConfig) error {
	if ctx.Pricer == nil {
		return fmt.Errorf("uniswap_v2: pricer not configured")
	}
	uv2Cfg, err := buildUniswapV2Config(*dexCfg, ctx.Config.ResolveSharedPoolsPath(), ctx.Assets)
	if err != nil {
		return err
	}
	dialer := gorillaDialer{}
	connector := uniswap.NewConnector(uv2Cfg, dialer, ctx.Pricer)
	name := uv2Cfg.Exchange
	if name == "" {
		name = "uniswap_v2"
	}
	ctx.Supervisor(name, func() error {
		cCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-ctx.Stop:
				cancel()
			case <-cCtx.Done():
			}
		}()
		return connector.Run(cCtx, ctx.DataChannel)
	})
	return nil
}

func buildUniswapV3(ctx LaunchContext, _ string, _ []string, dexCfg *config.DexConfig) error {
	if ctx.Pricer == nil {
		return fmt.Errorf("uniswap_v3: pricer not configured")
	}
	v3Cfg, err := buildUniswapV3Config(*dexCfg, ctx.Config.ResolveSharedPoolsPath(), ctx.Assets)
	if err != nil {
		return err
	}
	connector := uniswap.NewV3Connector(v3Cfg, ctx.Pricer)
	name := v3Cfg.Exchange
	if name == "" {
		name = "uniswap_v3"
	}
	ctx.Supervisor(name, func() error {
		cCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-ctx.Stop:
				cancel()
			case <-cCtx.Done():
			}
		}()
		return connector.Run(cCtx, ctx.DataChannel)
	})
	return nil
}

func buildUniswapV2Config(dexCfg config.DexConfig, sharedPools string, assetsProvider *assets.Provider) (uniswap.Config, error) {
	wsURL := dexCfg.WSURL
	if wsURL == "" {
		return uniswap.Config{}, fmt.Errorf("uniswap_v2: ws_url empty")
	}
	httpURL := dexCfg.HTTPURL
	if httpURL == "" {
		return uniswap.Config{}, fmt.Errorf("uniswap_v2: http_url empty")
	}
	path := dexCfg.ResolvePoolsPath(sharedPools)
	registry := uniswap.NewTokenRegistry(assetsProvider, dexCfg.Network)
	pools, err := uniswap.LoadPoolsFromGeckoWithRegistry(path, registry)
	if err != nil {
		return uniswap.Config{}, err
	}
	for i := range pools {
		uniswap.FinalizePool(&pools[i])
	}
	batch := dexCfg.SubscribeBatch
	if batch <= 0 {
		batch = 150
	}
	ping := time.Duration(dexCfg.PingInterval)
	if ping <= 0 {
		ping = 25
	}
	return uniswap.Config{
		WSURL:              wsURL,
		HTTPURL:            httpURL,
		Exchange:           dexCfg.Name,
		Pools:              pools,
		SubscribeBatchSize: batch,
		PingInterval:       ping * time.Second,
	}, nil
}

func buildUniswapV3Config(dexCfg config.DexConfig, sharedPools string, assetsProvider *assets.Provider) (uniswap.V3Config, error) {
	wsURL := dexCfg.WSURL
	httpURL := dexCfg.HTTPURL
	if wsURL == "" || httpURL == "" {
		return uniswap.V3Config{}, fmt.Errorf("uniswap_v3: ws/http url empty")
	}
	path := dexCfg.ResolvePoolsPath(sharedPools)
	if path == "" {
		return uniswap.V3Config{}, fmt.Errorf("uniswap_v3: pools path empty")
	}
	ps := dexCfg.PoolsSource
	registry := uniswap.NewTokenRegistry(assetsProvider, dexCfg.Network)
	cfg := uniswap.V3Config{
		Exchange:       dexCfg.Name,
		WSURL:          wsURL,
		HTTPURL:        httpURL,
		PoolsPath:      path,
		DexFilter:      ps.GeckoDex,
		NetworkFilter:  ps.GeckoNetwork,
		BatchSize:      dexCfg.SubscribeBatch,
		PingInterval:   time.Duration(dexCfg.PingInterval) * time.Second,
		StopOnAckError: dexCfg.StopOnAckError,
		LogAllEvents:   dexCfg.LogAllEvents,
		DecodeSwapOnly: dexCfg.SwapOnly,
		MaxMetaWorkers: dexCfg.MaxMetaWorkers,
		Registry:       registry,
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 150
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 25 * time.Second
	}
	return cfg, nil
}

type gorillaDialer struct{}

func (gorillaDialer) Dial(ctx context.Context, endpoint string) (uniswap.WSConnection, error) {
	d := *websocket.DefaultDialer
	d.HandshakeTimeout = 15 * time.Second
	conn, _, err := d.DialContext(ctx, endpoint, nil)
	if err != nil {
		return nil, err
	}
	return &gorillaConn{conn: conn}, nil
}

type gorillaConn struct {
	conn *websocket.Conn
}

func (g *gorillaConn) WriteJSON(v interface{}) error {
	return g.conn.WriteJSON(v)
}

func (g *gorillaConn) ReadMessage() (int, []byte, error) {
	return g.conn.ReadMessage()
}

func (g *gorillaConn) WriteMessage(messageType int, data []byte) error {
	return g.conn.WriteMessage(messageType, data)
}

func (g *gorillaConn) Close() error {
	return g.conn.Close()
}
