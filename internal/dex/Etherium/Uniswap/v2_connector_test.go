package uniswap

import (
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

const (
	spxWethPoolAddr = "0x52c77b0cb827afbad022e6d6caf2c44452edbc39"
	syncPayload     = "0x00000000000000000000000000000000000000000000009314b78a7f6a0ecc14000000000000000000000000000000000000000000000000000383e85514185b0000000000000000000000000000000000000000000000000000000068de8ff7"
)

func TestComputeSnapshotBaseVsQuote(t *testing.T) {
	reserve0, _ := new(big.Int).SetString("9314b78a7f6a0ecc14", 16) // WETH reserve
	reserve1, _ := new(big.Int).SetString("000383e85514185b", 16)   // SPX reserve

	pool := PoolConfig{
		Address: common.HexToAddress(spxWethPoolAddr),
		Token0: TokenMeta{
			Symbol:   "WETH",
			Decimals: 18,
			IsWETH:   true,
		},
		Token1: TokenMeta{
			Symbol:   "SPX",
			Decimals: 8,
		},
		BaseIsToken0: false,
	}
	FinalizePool(&pool)

	snap, err := computeSnapshot(pool, syncPayload)
	if err != nil {
		t.Fatalf("computeSnapshot error: %v", err)
	}
	price, ok := ratToFloat(snap.Price)
	if !ok {
		t.Fatalf("invalid price: %v", snap.Price)
	}

	expected := ratio(reserve1, pool.Token1.Decimals, reserve0, pool.Token0.Decimals)
	expectedFloat, _ := ratToFloat(expected)
	if math.Abs(price-expectedFloat) > 1e-12 {
		t.Fatalf("unexpected price: got %.12f want %.12f", price, expectedFloat)
	}
}
