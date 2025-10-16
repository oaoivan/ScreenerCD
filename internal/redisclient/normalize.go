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
	priceMigrated, canonMigrated := r.migrateLegacyIdentityKeys()
	priceCount, err := r.normalizeKeyGroup("price")
	if err != nil {
		util.Errorf("redis normalize price keys error: %v", err)
	}
	canonCount, err := r.normalizeKeyGroup("price_canon")
	if err != nil {
		util.Errorf("redis normalize price_canon keys error: %v", err)
	}
	totalMigrated := priceMigrated + canonMigrated
	if priceCount+canonCount+totalMigrated > 0 {
		util.Infof("redis normalized exchange keys: price=%d, price_canon=%d migrated_price=%d migrated_canon=%d", priceCount, canonCount, priceMigrated, canonMigrated)
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

	var dest string
	switch prefix {
	case "price":
		if len(parts) >= 5 {
			networkNorm := util.NormalizeNetworkName(parts[1], 0)
			dexNorm := util.NormalizeSymbolDex(parts[2])
			ammNorm := util.NormalizeSymbolAMM(parts[3])
			symbol := strings.TrimSpace(parts[4])
			dest = fmt.Sprintf("%s:%s:%s:%s:%s", prefix, networkNorm, dexNorm, ammNorm, symbol)
			return r.mergeAndDelete(key, dest, networkNorm, dexNorm, symbol)
		}
		return 0, nil
	case "price_canon":
		if len(parts) >= 5 {
			networkNorm := util.NormalizeNetworkName(parts[1], 0)
			dexNorm := util.NormalizeSymbolDex(parts[2])
			ammNorm := util.NormalizeSymbolAMM(parts[3])
			canon := strings.TrimSpace(parts[4])
			dest = fmt.Sprintf("%s:%s:%s:%s:%s", prefix, networkNorm, dexNorm, ammNorm, canon)
			return r.mergeAndDelete(key, dest, networkNorm, dexNorm, "")
		}
		return 0, nil
	default:
		return 0, nil
	}
}

func (r *RedisClient) migrateLegacyIdentityKeys() (int, int) {
	price, err := r.migrateLegacyPriceKeys()
	if err != nil {
		util.Errorf("redis migrate legacy price keys error: %v", err)
	}
	canon, err := r.migrateLegacyCanonKeys()
	if err != nil {
		util.Errorf("redis migrate legacy price_canon keys error: %v", err)
	}
	return price, canon
}

type identitySegments struct {
	network string
	dex     string
	amm     string
	key     string
}

func (r *RedisClient) migrateLegacyPriceKeys() (int, error) {
	var migrated int
	var cursor uint64
	for {
		keys, next, err := r.client.Scan(r.ctx, cursor, "price:*", 512).Result()
		if err != nil {
			return migrated, err
		}
		for _, key := range keys {
			parts := strings.Split(key, ":")
			if len(parts) >= 5 {
				continue
			}
			fields, err := r.client.HGetAll(r.ctx, key).Result()
			if err != nil {
				util.Debugf("redis migrate skip key=%s err=%v", key, err)
				continue
			}
			identity, symbol, ok := inferPriceIdentity(parts, fields)
			if !ok {
				continue
			}
			destKey := fmt.Sprintf("price:%s:%s:%s:%s", identity.network, identity.dex, identity.amm, symbol)
			fields["network"] = identity.network
			fields["dex"] = identity.dex
			fields["amm_version"] = identity.amm
			fields["pool_identity"] = identity.key
			if err := r.client.HSet(r.ctx, destKey, flattenFields(fields)...).Err(); err != nil {
				util.Debugf("redis migrate unable to write key=%s err=%v", destKey, err)
				continue
			}
			if destKey != key {
				if _, err := r.client.Del(r.ctx, key).Result(); err != nil {
					util.Debugf("redis migrate unable to delete old key=%s err=%v", key, err)
					continue
				}
			}
			migrated++
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return migrated, nil
}

func (r *RedisClient) migrateLegacyCanonKeys() (int, error) {
	var migrated int
	var cursor uint64
	for {
		keys, next, err := r.client.Scan(r.ctx, cursor, "price_canon:*", 512).Result()
		if err != nil {
			return migrated, err
		}
		for _, key := range keys {
			parts := strings.Split(key, ":")
			if len(parts) >= 5 {
				continue
			}
			fields, err := r.client.HGetAll(r.ctx, key).Result()
			if err != nil {
				util.Debugf("redis migrate canon skip key=%s err=%v", key, err)
				continue
			}
			identity, canon, ok := inferCanonIdentity(parts, fields)
			if !ok {
				continue
			}
			destKey := fmt.Sprintf("price_canon:%s:%s:%s:%s", identity.network, identity.dex, identity.amm, canon)
			fields["network"] = identity.network
			fields["dex"] = identity.dex
			fields["amm_version"] = identity.amm
			fields["pool_identity"] = identity.key
			if err := r.client.HSet(r.ctx, destKey, flattenFields(fields)...).Err(); err != nil {
				util.Debugf("redis migrate canon unable to write key=%s err=%v", destKey, err)
				continue
			}
			if destKey != key {
				if _, err := r.client.Del(r.ctx, key).Result(); err != nil {
					util.Debugf("redis migrate canon unable to delete old key=%s err=%v", key, err)
					continue
				}
			}
			migrated++
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return migrated, nil
}

func inferPriceIdentity(parts []string, fields map[string]string) (identitySegments, string, bool) {
	if len(parts) < 3 {
		return identitySegments{}, "", false
	}
	symbol := strings.TrimSpace(parts[len(parts)-1])
	exchangeRaw := ""
	networkRaw := ""
	if len(parts) >= 4 {
		networkRaw = parts[1]
		exchangeRaw = parts[2]
	} else {
		exchangeRaw = parts[1]
	}
	if exchangeRaw == "" {
		exchangeRaw = fields["exchange"]
	}
	networkField := fields["network"]
	chainID := parseChainID(fields["chain_id"])
	segments := resolveIdentity(exchangeRaw, fields["dex"], fields["amm_version"], networkRaw, networkField, chainID)
	if segments.key == "" {
		return identitySegments{}, "", false
	}
	return segments, symbol, symbol != ""
}

func inferCanonIdentity(parts []string, fields map[string]string) (identitySegments, string, bool) {
	if len(parts) < 3 {
		return identitySegments{}, "", false
	}
	canon := strings.TrimSpace(parts[len(parts)-2])
	exchangeRaw := parts[len(parts)-1]
	networkRaw := ""
	if len(parts) >= 4 {
		networkRaw = parts[1]
	}
	if exchangeRaw == "" {
		exchangeRaw = fields["exchange"]
	}
	networkField := fields["network"]
	chainID := parseChainID(fields["chain_id"])
	segments := resolveIdentity(exchangeRaw, fields["dex"], fields["amm_version"], networkRaw, networkField, chainID)
	if segments.key == "" {
		return identitySegments{}, "", false
	}
	return segments, canon, canon != ""
}

func resolveIdentity(exchangeRaw, dexField, ammField, networkPart, networkField string, chainID uint64) identitySegments {
	identity := identitySegments{}
	network := networkField
	if network == "" {
		network = networkPart
	}
	identity.network = util.NormalizeNetworkName(network, chainID)
	dex := dexField
	if dex == "" {
		dex = exchangeRaw
	}
	identity.dex = util.NormalizeMarketDex(dex, exchangeRaw)
	amm := ammField
	identity.amm = util.NormalizeMarketAMM(amm, exchangeRaw)
	if identity.amm == "" {
		identity.amm = util.DefaultAMMForDex(identity.dex)
	}
	if identity.dex == "" {
		identity.dex = util.NormalizeMarketDex(exchangeRaw, exchangeRaw)
	}
	if identity.network == "" {
		identity.network = util.NormalizeNetworkName(networkPart, chainID)
	}
	if identity.dex == "" || identity.amm == "" || identity.network == "" {
		identity.key = ""
		return identity
	}
	identity.key = fmt.Sprintf("%s|%s|%s", identity.dex, identity.amm, identity.network)
	return identity
}

func parseChainID(raw string) uint64 {
	if raw == "" {
		return 0
	}
	if val, err := strconv.ParseUint(raw, 10, 64); err == nil {
		return val
	}
	return 0
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
	order := []string{"price", "timestamp", "exchange", "symbol", "network", "chain_id", "dex", "amm_version", "pool_identity"}
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
