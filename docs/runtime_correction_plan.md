# Runtime Correction Plan (2025‑10‑17)

## 1. Redis HSET Pipeline Fix
- Reproduce the `ERR wrong number of arguments for 'hset'` triggered by `internal/redisclient/client.go`.
- Audit `HSetBatch` to ensure we call `HSet(key, field, value)` or use `HMSet` with proper argument layout (`key`, `field`, `value`, ...).
- Add a unit test around the batching helper to catch malformed argument lists.
- Re-run `go test ./internal/redisclient/...` and a short screener session to confirm the pipeline writes successfully.

## 2. Base Pools Data Corrections
- Update the generator for `ticker_source/base_pools.json` so every Uniswap V3 entry has a valid `pool_id`. Where the external source lacks it, fallback to `pool_address` or fetch from on-chain metadata.
- Rebuild the dataset and verify that Uniswap V3 can load at least a non-zero set (check logs after launch: `uniswap_v3 loaded pools>0`).

## 3. Uniswap V4 Metadata Completeness
- Investigate entries skipped with `token0: empty address` and missing hooks. Ensure the dataset writes `token0.address`/`token1.address` and `pool_key.hooks` when applicable.
- Regenerate `base_pools.json`; confirm V4 bootstrap keeps the expected pool count (log `bootstrap pools done total=...`).

## 4. Regression Validation
- After applying the fixes, run `go test ./...` and launch `screener-core` locally.
- Observe startup logs (filters, pool counts) and ensure CEX+DEX connectors emit prices without Redis errors.
- Document the outcome in `docs/base_pools_migration_plan.md` (stage 9 follow-up).
