package main

import (
    "fmt"
    "math/big"

    uniswap "github.com/yourusername/screner/internal/dex/Etherium/Uniswap"
)

func main() {
    reserve0, _ := new(big.Int).SetString("9314b78a7f6a0ecc14", 16)
    reserve1, _ := new(big.Int).SetString("000383e85514185b", 16)
    pool := uniswap.PoolConfig{
        Token0: uniswap.TokenMeta{Symbol: "WETH", Decimals: 18, IsWETH: true},
        Token1: uniswap.TokenMeta{Symbol: "SPX", Decimals: 8},
        BaseIsToken0: false,
    }
    rat := uniswap.Ratio(reserve1, pool.Token1.Decimals, reserve0, pool.Token0.Decimals)
    fmt.Println(rat.FloatString(10))
}
