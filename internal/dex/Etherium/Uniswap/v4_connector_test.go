package uniswap

import (
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/yourusername/screner/internal/dex/pricing"
	pb "github.com/yourusername/screner/pkg/protobuf"
)

type stubPricer struct {
	resolve map[string]pricing.Result
}

func (s *stubPricer) RegisterToken(pricing.TokenInfo)                                              {}
func (s *stubPricer) RegisterStable(pricing.TokenInfo)                                             {}
func (s *stubPricer) UpdatePair(pricing.TokenInfo, pricing.TokenInfo, float64, float64, time.Time) {}

func (s *stubPricer) ResolveUSD(info pricing.TokenInfo) (pricing.Result, bool) {
	if s.resolve == nil {
		return pricing.Result{}, false
	}
	symbol := strings.ToUpper(strings.TrimSpace(info.Symbol))
	res, ok := s.resolve[symbol]
	return res, ok
}

func makeTestConnector(t *testing.T, exchange string, pricer pricing.Pricer) *V4Connector {
	t.Helper()
	cfg := V4Config{
		Exchange:       exchange,
		WSURL:          "wss://example",
		HTTPURL:        "https://example",
		PoolManager:    common.HexToAddress("0x0000000000000000000000000000000000000001"),
		SubscribeBatch: 1,
		PingInterval:   time.Second,
		MaxMetaWorkers: 1,
		Pools:          []V4PoolConfig{},
	}
	conn, err := NewV4Connector(cfg, pricer)
	if err != nil {
		t.Fatalf("new connector: %v", err)
	}
	return conn
}

func TestUpdatePricingEmitsSpotWithCanonicalExchange(t *testing.T) {
	pricer := &stubPricer{}
	connector := makeTestConnector(t, "", pricer)

	ch := make(chan *pb.MarketData, 2)
	meta := V4PoolConfig{
		Token0: TokenMeta{Symbol: "Uni-Swap", Address: common.HexToAddress("0x1"), Decimals: 18},
		Token1: TokenMeta{Symbol: "usd.c", Address: common.HexToAddress("0x2"), Decimals: 6},
	}

	connector.updatePricing(meta, 5.25, true, 0.19, true, 100, time.Unix(100, 0), ch)

	select {
	case md := <-ch:
		if md.Exchange != defaultExchangeName {
			t.Fatalf("expected exchange %q, got %q", defaultExchangeName, md.Exchange)
		}
		if md.Symbol != "UNISWAPUSDC" {
			t.Fatalf("unexpected symbol: %s", md.Symbol)
		}
	case <-time.After(time.Second):
		t.Fatal("no market data emitted")
	}
}

func TestUpdatePricingRespectsCustomExchangeName(t *testing.T) {
	pricer := &stubPricer{}
	connector := makeTestConnector(t, "UniV4-Custom", pricer)

	ch := make(chan *pb.MarketData, 2)
	meta := V4PoolConfig{
		Token0: TokenMeta{Symbol: "Lin-K", Address: common.HexToAddress("0x3"), Decimals: 18},
		Token1: TokenMeta{Symbol: "usd_t", Address: common.HexToAddress("0x4"), Decimals: 6},
	}

	connector.updatePricing(meta, 7.5, true, 0.133, true, 50, time.Unix(200, 0), ch)

	select {
	case md := <-ch:
		if md.Exchange != "univ4-custom" {
			t.Fatalf("expected lower-cased exchange, got %q", md.Exchange)
		}
		if md.Symbol != "LINKUSDT" {
			t.Fatalf("unexpected symbol %s", md.Symbol)
		}
	case <-time.After(time.Second):
		t.Fatal("no market data emitted")
	}
}

func TestEmitUSDUsesExchangeName(t *testing.T) {
	pricer := &stubPricer{resolve: map[string]pricing.Result{
		"UNI-SWAP": {Price: 1.12, Weight: 10, Route: []string{"USDC"}},
	}}
	connector := makeTestConnector(t, "", pricer)

	ch := make(chan *pb.MarketData, 1)
	connector.emitUSD(ch, pricing.TokenInfo{Symbol: "uni-swap", Address: common.HexToAddress("0x1"), Decimals: 18}, time.Unix(300, 0))

	select {
	case md := <-ch:
		if md.Exchange != defaultExchangeName {
			t.Fatalf("expected exchange %q, got %q", defaultExchangeName, md.Exchange)
		}
		if md.Symbol != "UNISWAPUSD" {
			t.Fatalf("unexpected symbol %s", md.Symbol)
		}
	case <-time.After(time.Second):
		t.Fatal("no USD market data emitted")
	}
}
