package main

import (
    "context"
    "log"

    redis "github.com/go-redis/redis/v8"
)

func main() {
    ctx := context.Background()
    client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6380"})
    defer client.Close()

    log.Printf("ping redis err=%v", client.Ping(ctx).Err())

    infoServer := client.Info(ctx, "server")
    serverInfo, infoErr := infoServer.Result()
    log.Printf("redis info server err=%v", infoErr)
    log.Printf("redis info server=%q", serverInfo)

    infoCmd := client.Do(ctx, "COMMAND", "INFO", "HSET")
    cmdResult, cmdErr := infoCmd.Result()
    log.Printf("redis command info err=%v", cmdErr)
    log.Printf("redis command info=%#v", cmdResult)

    payload := []interface{}{"HSET", "test:hsetbatch", "price", "11.5", "timestamp", "1234567890", "exchange", "okx", "symbol", "ILV-USDT"}
    log.Printf("payload len=%d", len(payload))

    err := client.Do(ctx, payload...).Err()
    log.Printf("hset batch err=%v", err)
}
