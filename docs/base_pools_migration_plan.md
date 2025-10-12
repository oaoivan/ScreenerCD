# Base Pools Source Migration Plan

## 1. Preparation And Architecture Alignment
- Confirm the existing flow: `ticker_source/*.json` feeds Uniswap loaders (`internal/dex/Etherium/Uniswap/*`), which push prices to Redis via `internal/redisclient`; desktop UI reads from Redis.
- Capture reference snapshots from the legacy GeckoTerminal export and `ticker_source/base_pools.json` (counts, key fields, presence of `amm_version`).
- Inventory services, launchers, scripts, and docs that point at the legacy path so the switch cannot silently break the pipeline.

## 2. Schema And Compatibility Review
- Compare `entries[*]` payloads to ensure shared fields (`token0`, `token1`, `pair_name`, `pool_address`, `network`, `pool_key`) remain aligned.
- Record differences: new `amm_version`, absent `base_token`/`quote_token`, sparse `pool_manager`, missing `pool_id`, and many `null` values for `pool_key.fee`/`tickSpacing`.
- Design DTO/parsers that prefer `amm_version` but fall back to `dex` for older bundles.

## 3. Config And Loader Updates
- Point configuration (e.g. `configs/screener-core.yaml`) to `ticker_source/base_pools.json`.
- Refactor Uniswap loaders (V2/V3/V4) to filter by `amm_version` (`v2`/`v3`/`v4`) with graceful fallback to `dex`.
- Wrap or rename helpers (`LoadPoolsFromGecko*`) so the shared parser understands both formats during rollout.
- Update launcher wiring (`internal/launcher/register_dex.go`) and supporting scripts to avoid hard coded references to the old file.

## 4. Pipeline Validation
- Run unit/integration coverage for Uniswap connectors to confirm Redis still receives valid pool metadata.
- Replay local subscription scripts (`test_scripts/uniswap/*`) against the new source and confirm UI consumers stay healthy.
- Add guard rails for missing `amm_version`: log clearly, fall back to `dex`, and skip safely when neither is available.

## 5. Documentation And Rollout Support
- Refresh `docs/project_documentation.md`, `README`s, and integration notes with the new source name and filtering rules.
- Keep a rollback path: archive the legacy JSON and expose a temporary config flag for emergency rollback.
- Notify the team with a changelog entry plus smoke test instructions before and after deployment.

## 6. Deployment
- Roll changes through dev and staging first, monitoring Redis logs and connector health.
- Promote to production only after validation, then watch subscription metrics and parser warnings for at least 24 hours.

## 7. Risks And Mitigations
- **Missing `amm_version`:** rely on `dex` and emit a structured warning.
- **`pool_key` gaps:** default sensible values for V2 or skip pools lacking mandatory data.
- **Unexpected DEX labels:** centralise mapping `dex` > supported connector and fail fast at startup when the mapping is unknown.

## Step 1 Findings
- **Pipeline snapshot:** launch context (`internal/launcher/register_dex.go`) feeds Uniswap connectors (`v2_connector.go`, `connect_uniswap_v3_all.go`, `v4_connector.go`) which publish quotes into Redis (`internal/redisclient`). UI components read from Redis; matches `docs/project_documentation.md`.
- **Data snapshots:** legacy Gecko export holds 301 entries; `ticker_source/base_pools.json` holds 875. New feed adds `amm_version`, drops `base_token`/`quote_token`, sparse `pool_manager` (11 vs 34) and omits `pool_id` entirely. Shared fields remain (`symbol`, `network`, `pair_name`, `pool_address`, `liquidity_usd`, `token0`, `token1`, `pool_key`), while `fee_percent` stays optional (182/301 vs 120/875).
- **Current consumers:** direct path references live in `configs/screener-core.yaml`, Uniswap connectors (`internal/dex/Etherium/Uniswap/*`), launcher wiring (`internal/launcher/register_dex.go`), helper scripts (`test_scripts/uniswap/*`, `Temp/list_pools.go`), and documentation (`docs/project_documentation.md`, `docs/uniswap_v4_integration_plan.md`, `docs/v4_data_flow_report.txt`).

## Step 2 Findings
- **Field coverage:** `pool_key` structure remains identical, but `pool_key.fee`/`tickSpacing` are `null` for 864/875 entries; status `ok` is rare (11/875 vs 73/301 previously).
- **Identifiers:** `pool_id` disappears in the new feed (0 occurrences vs 73 previously); `pool_manager` now appears in only 11 records (mostly V4).
- **AMM versioning:** `amm_version` is populated for all but 8 entries (`v2`: 719, `v3`: 85, `v1`: 49, `v4`: 14, `unknown`: 8). Legacy file encoded version implicitly via `dex`.
- **DEX coverage:** new feed aggregates many venues (`uniswap`, `pancakeswap`, `aerodrome`, `curve`, etc.), so filtering logic must restrict processing to connectors we actually support.
- **Base/quote tokens:** fields were unused in code; no restoration required. Version detection and base/quote selection already rely on `token0`/`token1` metadata.

## Step 3 Findings
- **Config & code switch:** all loaders, launchers, scripts, and configs now read from `ticker_source/base_pools.json`; helpers renamed to `LoadPoolsFromSource*` with compatibility wrappers.
- **Parser upgrades:** V2/V3/V4 connectors honour `amm_version`, match Uniswap-specific `dex` labels, and in V4 derive `pool_id` from `pool_key` when absent.
- **Docs & tooling:** documentation and auxiliary scripts point to the new dataset; legacy path references were scrubbed to avoid regressions during deployment.
