# Uniswap V4 Integration Checklist

## 1. Inventory & Context
- Review the experimental scripts under `test_scripts/uniswap/v4/` to understand expected Swap payloads and PoolManager events.
- Audit the production connector (`internal/dex/Etherium/Uniswap/v4_connector.go`) and launcher wiring (`internal/launcher/register_dex.go`) to confirm that every required field is sourced from `base_pools`.
- Document auxiliary inputs (ABI files, RPC endpoints, Alchemy keys) so the environment variables remain explicit.

## 2. Base Pools Migration
- V4 now consumes `ticker_source/base_pools.json`. Launcher builders supply `basepools.Filter` derived from `DexConfig.PoolsSource` (`dex_filter`, `network_filter`, `amm_version_filter`). Only normalised `dex="uniswap"`, `amm_version="v4"` entries are loaded.
- Inline pools (`dexCfg.Pools`) remain available for hotfixes; the converter validates `pool_id`, `pool_address`, `hook_address`, and `tick_spacing` before merging with the shared dataset.

## 3. Connector Expectations
- `V4Config` requires both WS and HTTP URLs plus a valid `pool_manager`. When `PoolsSource` is used, `DexConfig.Validate()` enforces that the filters are present.
- The connector verifies `pool_key` metadata (tick spacing, hooks) and refreshes token decimals via `eth_call` when `base_pools` metadata is incomplete.
- Events are received from the PoolManager topic; swaps are normalised and forwarded to the graph pricer (`RegisterToken`, `UpdatePair`, `ResolveUSD`).

## 4. Logging & Metrics
- Launcher logs (util.Infof) print the path, filters, and number of pools loaded or merged. This makes it easy to confirm that `base_pools` is the active source.
- Runtime metrics expose per-connector message counts and Redis throughput; inspect `screner.log` to verify that V4 contributes data after launch.

## 5. Validation Steps
1. Run `go test ./internal/dex/Etherium/Uniswap/...` to ensure the connector compiles and unit tests pass.
2. Launch `cmd/screener-core` with V4 enabled; confirm the startup log mentions the expected `base_pools` path, filter set, and pool counts.
3. Inspect Redis (`HGETALL price:uniswap:WETHUSDC` or similar) to confirm that swaps produce USD quotes.
4. Keep the documentation (`docs/project_documentation.md`, this file, and README) in sync with the latest pipeline description.
