# ScreenerCD Architecture Guide

## 1. Runtime Overview
- **Core service**: `cmd/screener-core/main.go` parses the YAML config, prepares Redis clients, and wires launcher builders from `internal/launcher`. Launchers own the lifecycle of every exchange connector and push data into a shared channel.
- **Configuration schema**: `internal/config/config.go` defines Redis settings, per-connector options, shared assets registry, and helper flags (stop-on-ack-error, swap-only, Bitget rate limits, etc.).
- **Data models**: Common structs live in `pkg/common/models.go`; protobuf definitions are under `pkg/protobuf`. Connectors exchange `protobuf.MarketData` messages over the shared channel.

## 2. Data Flow Pipeline
1. **Ingestion** – each connector subscribes to its exchange (WS or RPC), normalises payloads, and emits `MarketData` into the channel.
2. **Redis workers** – background goroutines batch `HSET` operations, keep metrics, and expose health information.
3. **Pricer & metrics** – `internal/dex/pricing` maintains USD anchors and resolves derived quotes. Periodic metrics summarise throughput and connector status.
4. **Shutdown** – POSIX signals propagate through `LaunchContext`, cancelling connectors and draining the worker pool before exit.

## 3. Exchange Connectors
- **CEX (Bybit / Gate / Bitget / OKX)**: live in `internal/exchange`, subscribe to native ticker feeds, apply per-exchange throttling, and publish standardised prices.
- **Uniswap V2 (Ethereum)**: loads pools from `ticker_source/base_pools.json` via `basepools.Filter` (dex, network, optional amm version). Missing metadata (token decimals) is patched through `eth_call` before swaps from `Sync` events are processed.
- **Uniswap V3 (Ethereum)**: shares the same base-pools pipeline. `DexConfig.PoolsSource` provides `dex_filter`, `network_filter`, and `amm_version_filter`. The connector validates `pool_id`/`pool_address`, refreshes token metadata, and streams `Swap` events.
- **Uniswap V4 (Base/Ethereum)**: combines inline pools from config with filtered entries from `base_pools`. Only `dex="uniswap"` + `amm_version="v4"` records survive. The connector validates `pool_manager`, `pool_key` fields (tick spacing, hooks), subscribes to PoolManager events, and updates the graph pricer.

## 4. Symbol and Pool Datasets
- **CEX symbols**: defined per exchange in the YAML config. Each block can provide inline pairs, point to a `symbols_file`, or inherit `default_symbols_file` when omitted.
- **DEX pools**: resolved from `ticker_source/base_pools.json` unless overridden by inline pools in `DexConfig.Pools`. Launcher builders feed `basepools.Filter` with `DexFilter`, `NetworkFilter`, and `AmmVersionFilter`, ensuring that connector inputs match the normalised `base_pools` schema.

## 5. Configuration Touchpoints
- `DexConfig.Validate()` enforces `dex_filter` and `amm_version_filter` whenever an external base-pools source is used.
- `buildUniswapV[2|3|4]Config` log the resolved pool source, active filters, and resulting pool counts, making it easy to audit launch-time decisions.
- Graph pricer anchors (`reference_pools` in `configs/screener-core.yaml`) must use normalised dex names (e.g. `uniswap`, `pancakeswap`) to stay aligned with `basepools.NormalizeDex`.

## 6. Graph Pricer Highlights
- `internal/dex/pricing/graph_pricer.go` registers stable anchors from the assets registry (or fallback list) and tracks directed edges between tokens.
- `UpdatePair` converts pool updates into weighted graph edges; `ResolveUSD` resolves prices using at most three hops.
- Stable anchors and canonical pools should be kept in sync with the reference datasets above to guarantee consistent USD pricing.
