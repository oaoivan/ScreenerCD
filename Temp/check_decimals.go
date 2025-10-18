package main

import (
    "context"
    "fmt"
    "log"
    "math/big"
    "os"

    "github.com/ethereum/go-ethereum"
    "github.com/ethereum/go-ethereum/common"
    "github.com/ethereum/go-ethereum/ethclient"
)

func main() {
    key := os.Getenv("ALCHEMY_API_KEY")
    if key == "" {
        log.Fatal("ALCHEMY_API_KEY not set")
    }
    client, err := ethclient.Dial("https://eth-mainnet.g.alchemy.com/v2/" + key)
    if err != nil {
        log.Fatal(err)
    }
    defer client.Close()
    ctx := context.Background()
    addr := common.HexToAddress("0xe0f63a424a4439cbe457d80e4f4b51ad25b2c56c")
    msg := ethereum.CallMsg{To: &addr, Data: []byte{0x31, 0x3c, 0xe5, 0x67}}
    res, err := client.CallContract(ctx, msg, nil)
    if err != nil {
        log.Fatal(err)
    }
    if len(res) == 0 {
        log.Fatal("empty result")
    }
    dec := new(big.Int).SetBytes(res).Int64()
    fmt.Println("decimals:", dec)
}
