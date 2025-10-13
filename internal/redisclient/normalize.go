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
	var network string
	switch prefix {
	case "price":
		if len(parts) == 3 {
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
			return r.mergeAndDelete(key, dest, "", lower, destSymbol)
		}
		if len(parts) < 4 {
			return 0, nil
		}
		network = parts[1]
		exchange = strings.Join(parts[2:len(parts)-1], ":")
		if exchange == "" {
			return 0, nil
		}
		lower := util.NormalizeExchangeName(exchange)
		symbol := strings.TrimSpace(parts[len(parts)-1])
		destSymbol = symbol
		networkNorm := util.NormalizeNetworkName(network, 0)
		if lower == "gate" && !strings.Contains(destSymbol, "_") {
			destSymbol = util.BybitToGateSymbol(strings.ToUpper(destSymbol))
		}
		dest = fmt.Sprintf("%s:%s:%s:%s", prefix, networkNorm, lower, destSymbol)
		return r.mergeAndDelete(key, dest, networkNorm, lower, destSymbol)
	case "price_canon":
		if len(parts) == 3 {
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
			return r.mergeAndDelete(key, dest, "", lower, "")
		}
		if len(parts) < 4 {
			return 0, nil
		}
		network = parts[1]
		canon := parts[2]
		exchange = strings.Join(parts[3:], ":")
		if exchange == "" {
			return 0, nil
		}
		lower := util.NormalizeExchangeName(exchange)
		networkNorm := util.NormalizeNetworkName(network, 0)
		dest = fmt.Sprintf("%s:%s:%s:%s", prefix, networkNorm, canon, lower)
		return r.mergeAndDelete(key, dest, networkNorm, lower, "")
	default:
		return 0, nil
	}
}

func (r *RedisClient) mergeAndDelete(srcKey, destKey, network, exchangeLower, newSymbol string) (int, error) {
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
	if exchangeLower != "" {
		srcFields["exchange"] = exchangeLower
	}
	if network != "" {
		srcFields["network"] = network
	}
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
	} else {
		if exchangeLower != "" && destFields["exchange"] != exchangeLower {
			if err := r.client.HSet(r.ctx, destKey, "exchange", exchangeLower).Err(); err != nil {
				util.Debugf("redis normalize unable to update exchange for %s: %v", destKey, err)
			}
		}
		if newSymbol != "" && destFields["symbol"] != newSymbol {
			if err := r.client.HSet(r.ctx, destKey, "symbol", newSymbol).Err(); err != nil {
				util.Debugf("redis normalize unable to update symbol for %s: %v", destKey, err)
			}
		}
		if network != "" && destFields["network"] != network {
			if err := r.client.HSet(r.ctx, destKey, "network", network).Err(); err != nil {
				util.Debugf("redis normalize unable to update network for %s: %v", destKey, err)
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
	order := []string{"price", "timestamp", "exchange", "symbol", "network", "chain_id"}
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
