package main

import (
    "context"
    "fmt"

    redis "github.com/redis/go-redis/v9"
)

func main() {
    ctx := context.Background()
    rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379", Protocol: 2})
    defer rdb.Close()
    if err := rdb.HSet(ctx, "price:v9p2", "price", 0.12, "timestamp", 123, "exchange", "okx", "symbol", "BTCUSDT").Err(); err != nil {
        fmt.Println("HSET err:", err)
    } else {
        fmt.Println("HSET ok")
    }
}
