# Arbitrage Screener

## Overview
The Arbitrage Screener monitors price updates from multiple CEX and DEX venues, normalises the data, and exposes derived USD prices through Redis. The current implementation focuses on a single-user desktop workflow but keeps the ingestion core decoupled for future UI integrations.

## Project Structure
- **cmd/** – entrypoints (`screener-core`).
- **internal/** – core business logic (configuration, launchers, connectors, pricing, Redis workers).
- **pkg/** – protobuf definitions and shared models.
- **configs/** – deployment templates, including `screener-core.yaml`.
- **ticker_source/** – shared datasets (`base_pools.json`) consumed by DEX connectors.
- **scripts/**, **test_scripts/** – helper and diagnostic tools.

## Data Flow
1. `cmd/screener-core` loads `configs/screener-core.yaml`, builds launchers from `internal/launcher`, and allocates a shared channel.
2. Each connector pushes `protobuf.MarketData` into the channel; Redis workers batch-write `price:*` hashes.
3. `internal/dex/pricing` keeps stable anchors and resolves derived USD quotes using a graph search.
4. Metrics and health logs are printed via `util.Infof`/`util.Errorf`, allowing quick inspection of pool sources and message rates.

## Symbol & Pool Sources
- **CEX symbols**: each exchange block in the YAML config can list inline pairs, point to a `symbols_file`, or inherit `default_symbols_file`.
- **DEX pools**: launchers load `ticker_source/base_pools.json` through `basepools.Filter`. Filters come from `DexConfig.PoolsSource` (`DexFilter`, `NetworkFilter`, `AmmVersionFilter`). Inline overrides remain available via `DexConfig.Pools`.
- Keep reference pools (`reference_pools` in the config) aligned with `basepools.NormalizeDex` (e.g. `uniswap`, `pancakeswap`).

## Getting Started
```bash
git clone <repository-url>
cd screner
# Install Go (>=1.21) and Docker if you plan to run Redis locally

# Run unit tests
go test ./...

# Start Redis + screener-core
docker-compose up
```
`base_pools.json` ships with the repository; regenerate it with the internal tooling, then restart `screener-core` to apply new pools.

## References
- `docs/project_documentation.md` – architecture deep dive.
- `docs/uniswap_v4_integration_plan.md` – migration checklist for V4.
- `AGENTS.md` – coordination guidelines for automation.
