package redisclient

import (
	"context"
	"fmt"

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

// HSetBatch uses a pipeline to execute multiple HSET commands efficiently.
// entries is a slice of: key, field1, value1, field2, value2, ...
func (r *RedisClient) HSetBatch(entries [][]interface{}) error {
	if len(entries) == 0 {
		return nil
	}
	pipe := r.client.Pipeline()
	for _, e := range entries {
		if len(e) < 3 {
			// need at least key and one field/value pair
			continue
		}
		key, ok := e[0].(string)
		if !ok {
			util.Errorf("HSetBatch: first element must be key string, got %T", e[0])
			continue
		}
		fields := make(map[string]interface{}, (len(e)-1)/2)
		var invalid bool
		for i := 1; i < len(e); i += 2 {
			if i+1 >= len(e) {
				util.Errorf("HSetBatch: odd field count for key=%s payload=%v", key, e)
				invalid = true
				break
			}
			fieldName, ok := e[i].(string)
			if !ok {
				util.Errorf("HSetBatch: field name must be string for key=%s, got %T", key, e[i])
				invalid = true
				break
			}
			// Older Redis (<=3.x) only accepts HMSET for multiple fields; convert values to strings for consistency.
			fields[fieldName] = fmt.Sprint(e[i+1])
		}
		if invalid || len(fields) == 0 {
			continue
		}
		util.Debugf("PIPE HMSET %s %v", key, fields)
		pipe.HMSet(r.ctx, key, fields)
	}
	cmds, err := pipe.Exec(r.ctx)
	if err != nil {
		util.Errorf("HSetBatch pipeline exec error: %v", err)
		for _, cmd := range cmds {
			if cmdErr := cmd.Err(); cmdErr != nil {
				util.Errorf("HSetBatch command error: %v args=%v", cmdErr, cmd.Args())
			}
		}
	}
	return err
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
