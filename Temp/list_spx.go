package main

import (
    "fmt"
    "strings"

    uniswap "github.com/yourusername/screner/internal/dex/Etherium/Uniswap"
)

func main() {
    pools, err := uniswap.LoadPoolsFromBase("ticker_source/base_pools.json", "uniswap", "ethereum")
    if err != nil {
        panic(err)
    }
    for _, p := range pools {
        if strings.Contains(p.PairName, "SPX") {
            fmt.Printf("%s addr=%s baseIs0=%v t0=%s(%d) t1=%s(%d)\n", p.PairName, p.Address.Hex(), p.BaseIsToken0, p.Token0.Symbol, p.Token0.Decimals, p.Token1.Symbol, p.Token1.Decimals)
        }
    }
}
