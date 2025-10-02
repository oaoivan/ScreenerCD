package redisclient

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/yourusername/screner/internal/util"
)

// NormalizeExchangeKeys cleans legacy Redis hashes that used mixed-case exchange names.
// It merges them into lower-case equivalents and deletes duplicates.
func (r *RedisClient) NormalizeExchangeKeys() {
	priceCount, err := r.normalizeKeyGroup("price")
	if err != nil {
		util.Errorf("redis normalize price keys error: %v", err)
	}
	canonCount, err := r.normalizeKeyGroup("price_canon")
	if err != nil {
		util.Errorf("redis normalize price_canon keys error: %v", err)
	}
	if priceCount+canonCount > 0 {
		util.Infof("redis normalized exchange keys: price=%d, price_canon=%d", priceCount, canonCount)
	}
}

func (r *RedisClient) normalizeKeyGroup(prefix string) (int, error) {
	var normalized int
	var cursor uint64
	pattern := prefix + ":*"
	for {
		keys, next, err := r.client.Scan(r.ctx, cursor, pattern, 512).Result()
		if err != nil {
			return normalized, err
		}
		for _, key := range keys {
			count, err := r.normalizeKey(prefix, key)
			if err != nil {
				util.Debugf("redis normalize skip key=%s err=%v", key, err)
				continue
			}
			normalized += count
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return normalized, nil
}

func (r *RedisClient) normalizeKey(prefix, key string) (int, error) {
	parts := strings.Split(key, ":")
	if len(parts) < 3 {
		return 0, nil
	}

	var exchange string
	var dest string
	var destSymbol string
	switch prefix {
	case "price":
		exchange = strings.Join(parts[1:len(parts)-1], ":")
		if exchange == "" {
			return 0, nil
		}
		lower := util.NormalizeExchangeName(exchange)
		symbol := strings.TrimSpace(parts[len(parts)-1])
		destSymbol = symbol
		if lower == strings.ToLower(strings.TrimSpace(exchange)) {
			if lower != "gate" || strings.Contains(symbol, "_") {
				return 0, nil
			}
		}
		if lower == "gate" && !strings.Contains(destSymbol, "_") {
			destSymbol = util.BybitToGateSymbol(strings.ToUpper(destSymbol))
		}
		dest = fmt.Sprintf("%s:%s:%s", prefix, lower, destSymbol)
		return r.mergeAndDelete(key, dest, lower, destSymbol)
	case "price_canon":
		exchange = strings.Join(parts[2:], ":")
		if exchange == "" {
			return 0, nil
		}
		lower := util.NormalizeExchangeName(exchange)
		if lower == strings.ToLower(strings.TrimSpace(exchange)) {
			return 0, nil
		}
		canon := parts[1]
		dest = fmt.Sprintf("%s:%s:%s", prefix, canon, lower)
		return r.mergeAndDelete(key, dest, lower, "")
	default:
		return 0, nil
	}
}

func (r *RedisClient) mergeAndDelete(srcKey, destKey, exchangeLower, newSymbol string) (int, error) {
	if srcKey == destKey {
		return 0, nil
	}
	srcFields, err := r.client.HGetAll(r.ctx, srcKey).Result()
	if err != nil {
		return 0, err
	}
	if len(srcFields) == 0 {
		_, _ = r.client.Del(r.ctx, srcKey).Result()
		return 1, nil
	}
	srcFields["exchange"] = exchangeLower
	if newSymbol != "" {
		srcFields["symbol"] = newSymbol
	}

	destFields, err := r.client.HGetAll(r.ctx, destKey).Result()
	if err != nil {
		return 0, err
	}
	srcTs := parseTimestamp(srcFields)
	destTs := parseTimestamp(destFields)

	if len(destFields) == 0 || srcTs >= destTs {
		if err := r.client.HSet(r.ctx, destKey, flattenFields(srcFields)...).Err(); err != nil {
			return 0, err
		}
	} else if destFields["exchange"] != exchangeLower {
		if err := r.client.HSet(r.ctx, destKey, "exchange", exchangeLower).Err(); err != nil {
			util.Debugf("redis normalize unable to update exchange for %s: %v", destKey, err)
		}
		if newSymbol != "" {
			if err := r.client.HSet(r.ctx, destKey, "symbol", newSymbol).Err(); err != nil {
				util.Debugf("redis normalize unable to update symbol for %s: %v", destKey, err)
			}
		}
	}

	if _, err := r.client.Del(r.ctx, srcKey).Result(); err != nil {
		return 0, err
	}
	return 1, nil
}

func parseTimestamp(fields map[string]string) int64 {
	if v, ok := fields["timestamp"]; ok {
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
			return ts
		}
	}
	return 0
}

func flattenFields(fields map[string]string) []interface{} {
	order := []string{"price", "timestamp", "exchange", "symbol"}
	out := make([]interface{}, 0, len(fields)*2)
	seen := make(map[string]struct{}, len(fields))
	for _, key := range order {
		if val, ok := fields[key]; ok {
			out = append(out, key, val)
			seen[key] = struct{}{}
		}
	}
	for key, val := range fields {
		if _, ok := seen[key]; ok {
			continue
		}
		out = append(out, key, val)
	}
	return out
}
