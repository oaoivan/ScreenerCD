package main

import (
    "fmt"
    "math/big"

    "github.com/ethereum/go-ethereum/accounts/abi"
    "github.com/ethereum/go-ethereum/common"
    "golang.org/x/crypto/sha3"
)

func main() {
    token0 := common.HexToAddress("0xA0b86991c6218b36c1d19d4a2e9eb0ce3606eb48")
    token1 := common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")
    fees := []uint64{500, 3000, 10000}

    addressType, _ := abi.NewType("address", "", nil)
    feeType, _ := abi.NewType("uint24", "", nil)
    arguments := abi.Arguments{
        {Type: addressType},
        {Type: addressType},
        {Type: feeType},
    }

    for _, fee := range fees {
        packed, err := arguments.Pack(token0, token1, big.NewInt(int64(fee)))
        if err != nil {
            panic(err)
        }
        hash := keccak256(packed)
        fmt.Printf("fee %d key %s\n", fee, hash.Hex())
    }
}

func keccak256(data []byte) common.Hash {
    h := sha3.NewLegacyKeccak256()
    h.Write(data)
    var out common.Hash
    h.Sum(out[:0])
    return out
}
