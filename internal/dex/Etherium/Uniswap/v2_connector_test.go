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

func TestComputeSnapshotDependsOnTokenOrdering(t *testing.T) {
	reserve0, _ := new(big.Int).SetString("9314b78a7f6a0ecc14", 16)
	reserve1, _ := new(big.Int).SetString("000383e85514185b", 16)

	poolWrong := PoolConfig{
		Address: common.HexToAddress(spxWethPoolAddr),
		Token0: TokenMeta{
			Symbol:   "SPX",
			Decimals: 8,
		},
		Token1: TokenMeta{
			Symbol:   "WETH",
			Decimals: 18,
			IsWETH:   true,
		},
		BaseIsToken0: true,
	}
	FinalizePool(&poolWrong)

	snapWrong, err := computeSnapshot(poolWrong, syncPayload)
	if err != nil {
		t.Fatalf("computeSnapshot (wrong order) error: %v", err)
	}
	wrongPrice, ok := ratToFloat(snapWrong.Price)
	if !ok {
		t.Fatalf("invalid price for wrong order: %v", snapWrong.Price)
	}
	expectedWrong := new(big.Rat).Quo(
		new(big.Rat).SetFrac(reserve1, tenPow(uint(poolWrong.Token1.Decimals))),
		new(big.Rat).SetFrac(reserve0, tenPow(uint(poolWrong.Token0.Decimals))),
	)
	wrongExpectedFloat, _ := ratToFloat(expectedWrong)
	if math.Abs(wrongPrice-wrongExpectedFloat) > 1e-30 {
		t.Fatalf("unexpected wrong price: got %.30e want %.30e", wrongPrice, wrongExpectedFloat)
	}

	poolCorrect := poolWrong
	swapPoolTokens(&poolCorrect)
	poolCorrect.BaseIsToken0 = false
	FinalizePool(&poolCorrect)

	snapCorrect, err := computeSnapshot(poolCorrect, syncPayload)
	if err != nil {
		t.Fatalf("computeSnapshot (correct order) error: %v", err)
	}
	correctPrice, ok := ratToFloat(snapCorrect.Price)
	if !ok {
		t.Fatalf("invalid price for correct order: %v", snapCorrect.Price)
	}
	expectedCorrect := new(big.Rat).Quo(
		new(big.Rat).SetFrac(reserve0, tenPow(uint(poolCorrect.Token0.Decimals))),
		new(big.Rat).SetFrac(reserve1, tenPow(uint(poolCorrect.Token1.Decimals))),
	)
	correctExpectedFloat, _ := ratToFloat(expectedCorrect)
	if math.Abs(correctPrice-correctExpectedFloat) > 1e-18 {
		t.Fatalf("unexpected correct price: got %.18f want %.18f", correctPrice, correctExpectedFloat)
	}

	if correctPrice <= wrongPrice {
		t.Fatalf("correct price should be greater than wrong price got correct %.18f wrong %.30e", correctPrice, wrongPrice)
	}
}
