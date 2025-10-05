package pricing

import (
	"math"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func makeToken(hexAddr, symbol string, decimals int) TokenInfo {
	return TokenInfo{
		Address:  common.HexToAddress(hexAddr),
		Symbol:   symbol,
		Decimals: decimals,
	}
}

func TestResolveUSDChoosesHighestWeightStable(t *testing.T) {
	stables := []TokenInfo{
		makeToken("0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", "USDC", 6),
		makeToken("0xdac17f958d2ee523a2206206994597c13d831ec7", "USDT", 6),
	}
	pricer := NewGraphPricer(stables)

	asset := makeToken("0x1111111111111111111111111111111111111111", "ABC", 18)
	ts := time.Unix(1_700_000_000, 0).UTC()

	pricer.UpdatePair(asset, stables[0], 10.0, 50.0, ts.Add(-time.Minute))
	pricer.UpdatePair(asset, stables[1], 9.0, 120.0, ts)

	res, ok := pricer.ResolveUSD(asset)
	if !ok {
		t.Fatalf("expected USD route for asset")
	}

	if math.Abs(res.Price-9.0) > 1e-9 {
		t.Fatalf("unexpected price: got %.12f want 9.0", res.Price)
	}

	if math.Abs(res.Weight-120.0) > 1e-9 {
		t.Fatalf("unexpected weight: got %.12f want 120", res.Weight)
	}

	if len(res.Route) != 2 {
		t.Fatalf("unexpected route length: %v", res.Route)
	}

	if res.Route[0] != "ABC" || res.Route[1] != "USDT" {
		t.Fatalf("unexpected route: %v", res.Route)
	}
}

func TestResolveUSDPrefersFresherStableWhenWeightsEqual(t *testing.T) {
	stables := []TokenInfo{
		makeToken("0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", "USDC", 6),
		makeToken("0xdac17f958d2ee523a2206206994597c13d831ec7", "USDT", 6),
	}
	pricer := NewGraphPricer(stables)

	asset := makeToken("0x2222222222222222222222222222222222222222", "XYZ", 18)
	oldTS := time.Unix(1_700_000_000, 0).UTC()
	freshTS := oldTS.Add(2 * time.Minute)

	pricer.UpdatePair(asset, stables[0], 8.0, 75.0, oldTS)
	pricer.UpdatePair(asset, stables[1], 8.5, 75.0, freshTS)

	res, ok := pricer.ResolveUSD(asset)
	if !ok {
		t.Fatalf("expected USD route for XYZ")
	}

	if math.Abs(res.Price-8.5) > 1e-9 {
		t.Fatalf("unexpected price: got %.12f want 8.5", res.Price)
	}

	if len(res.Route) != 2 {
		t.Fatalf("unexpected route length: %v", res.Route)
	}

	if res.Route[0] != "XYZ" || res.Route[1] != "USDT" {
		t.Fatalf("unexpected route: %v", res.Route)
	}

	if !res.UpdatedAt.Equal(freshTS) {
		t.Fatalf("unexpected UpdatedAt: got %s want %s", res.UpdatedAt, freshTS)
	}
}
