package redisclient

import (
	"context"

	redis "github.com/go-redis/redis/v8"
	"github.com/yourusername/screner/internal/util"
)

type RedisClient struct {
	client *redis.Client
	ctx    context.Context
}

func NewRedisClient(addr string, password string, db int) *RedisClient {
	util.Infof("initializing Redis client: addr=%s db=%d", addr, db)
	rdb := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	ctx := context.Background()

	return &RedisClient{
		client: rdb,
		ctx:    ctx,
	}
}

func (r *RedisClient) Set(key string, value interface{}) error {
	util.Debugf("SET %s %v", key, value)
	err := r.client.Set(r.ctx, key, value, 0).Err()
	if err != nil {
		util.Errorf("SET error for key=%s: %v", key, err)
	}
	return err
}

func (r *RedisClient) Get(key string) (string, error) {
	util.Debugf("GET %s", key)
	res, err := r.client.Get(r.ctx, key).Result()
	if err != nil {
		util.Errorf("GET error for key=%s: %v", key, err)
	}
	return res, err
}

func (r *RedisClient) HSet(key string, values ...interface{}) error {
	util.Debugf("HSET %s %v", key, values)
	err := r.client.HSet(r.ctx, key, values...).Err()
	if err != nil {
		util.Errorf("HSET error for key=%s: %v", key, err)
	}
	return err
}

func (r *RedisClient) HGetAll(key string) (map[string]string, error) {
	util.Debugf("HGETALL %s", key)
	res, err := r.client.HGetAll(r.ctx, key).Result()
	if err != nil {
		util.Errorf("HGETALL error for key=%s: %v", key, err)
	}
	return res, err
}

func (r *RedisClient) Close() error {
	util.Infof("closing Redis client")
	return r.client.Close()
}

func (r *RedisClient) Ping() error {
	util.Debugf("PING Redis")
	err := r.client.Ping(r.ctx).Err()
	if err != nil {
		util.Errorf("PING error: %v", err)
	}
	return err
}
