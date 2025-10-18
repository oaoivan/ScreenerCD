package main

import (
    "fmt"

    "github.com/gomodule/redigo/redis"
)

func main() {
    conn, err := redis.Dial("tcp", "127.0.0.1:6379")
    if err != nil {
        panic(err)
    }
    defer conn.Close()
    res, err := conn.Do("HSET", "price:redigo", "price", "0.12", "timestamp", "123", "exchange", "okx", "symbol", "BTCUSDT")
    fmt.Println("res:", res, "err:", err)
}
