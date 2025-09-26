package main

import (
    "fmt"

    "golang.org/x/crypto/sha3"
)

func main() {
    sig := []byte("Initialize(bytes32,address,address,uint24,int24,address,uint160,int24)")
    h := sha3.NewLegacyKeccak256()
    h.Write(sig)
    var out [32]byte
    h.Sum(out[:0])
    fmt.Printf("0x%x\n", out)
}
