package main

import (
	"context"
	"fmt"

	redis "github.com/go-redis/redis/v8"
)

func redisPipelineTest() error {
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	pipe := rdb.Pipeline()
	pipe.HSet(ctx, "test:hash", "price", 0.7818, "timestamp", 1759507485, "exchange", "bitget", "symbol", "CRVUSDT")
	_, err := pipe.Exec(ctx)
	if err != nil {
		fmt.Println("pipeline error:", err)
		return err
	}
	fmt.Println("ok")
	return nil
}
