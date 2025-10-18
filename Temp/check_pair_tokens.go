package main

import (
    "context"
    "fmt"
    "log"
    "os"

    "github.com/ethereum/go-ethereum"
    "github.com/ethereum/go-ethereum/common"
    "github.com/ethereum/go-ethereum/ethclient"
)

func main() {
    key := os.Getenv("ALCHEMY_API_KEY")
    client, err := ethclient.Dial("https://eth-mainnet.g.alchemy.com/v2/" + key)
    if err != nil {
        log.Fatal(err)
    }
    defer client.Close()
    pair := common.HexToAddress("0x52c77b0cb827afbad022e6d6caf2c44452edbc39")
    call := func(data []byte) common.Address {
        res, err := client.CallContract(context.Background(), ethereum.CallMsg{To: &pair, Data: data}, nil)
        if err != nil {
            log.Fatal(err)
        }
        if len(res) < 32 {
            log.Fatalf("unexpected len %d", len(res))
        }
        return common.BytesToAddress(res[len(res)-20:])
    }
    fmt.Println("token0", call([]byte{0x0d, 0xfe, 0x16, 0x81}).Hex())
    fmt.Println("token1", call([]byte{0xd2, 0x12, 0x20, 0xa7}).Hex())
}
