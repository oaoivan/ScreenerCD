package main

import (
	"encoding/json"
	"fmt"
	"log"

	uniswap "github.com/yourusername/screner/internal/dex/Etherium/Uniswap"
)

func main() {
	pools, err := uniswap.LoadPoolsFromGecko("ticker_source/geckoterminal_pools.json")
	if err != nil {
		log.Fatal(err)
	}
	type output struct {
		Pair   string `json:"pair"`
		Canon  string `json:"canonical"`
		Stable bool   `json:"has_stable"`
		WETH   bool   `json:"has_weth"`
		Addr   string `json:"address"`
	}
	list := make([]output, 0, len(pools))
	for _, p := range pools {
		list = append(list, output{
			Pair:   p.PairName,
			Canon:  p.CanonicalPair,
			Stable: p.HasStable,
			WETH:   p.HasWETH,
			Addr:   p.Address.Hex(),
		})
	}
	// simple insertion sort by Pair for stable output
	for i := 1; i < len(list); i++ {
		j := i
		for j > 0 && list[j].Pair < list[j-1].Pair {
			list[j], list[j-1] = list[j-1], list[j]
			j--
		}
	}
	fmt.Printf("total=%d\n", len(list))
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(data))
}
