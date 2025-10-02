# ScreenerCD Architecture Guide

## 1. Runtime Overview
- **Core service**: `cmd/screener-core/main.go` bootstraps the runtime by loading YAML configuration, instantiating the Redis client, resolving the enabled exchanges, и теперь делегирует запуск коннекторов фабрике `internal/launcher` (реестр `Register/LaunchContext`). Добавление новой площадки требует лишь регистрации билдера без правок `main.go`. Символьные списки по-прежнему подготавливаются через shared pools/inline конфиг.【F:cmd/screener-core/main.go†L60-L156】【F:internal/launcher/registry.go†L1-L45】【F:internal/launcher/register_cex.go†L1-L167】
- **Configuration schema**: `internal/config/config.go` models Redis connectivity, symbol sources, and batching parameters, applying safe defaults for buffer sizes, Redis workers, and Bitget-specific rate limits when options are omitted.【F:internal/config/config.go†L11-L107】
- **Data structures**: Market snapshots are expressed through both Go structs (`pkg/common/models.go`) and Protocol Buffers (`pkg/protobuf/arbitrage.proto`) to keep serialization consistent across components.【F:pkg/common/models.go†L3-L18】【F:pkg/protobuf/arbitrage.proto†L5-L19】

## 2. Data Flow Pipeline
1. **Exchange ingestion** – Supervisors spawn a connector per exchange, subscribe in batches (respecting venue-specific throttling), and push parsed ticker updates into a shared buffered channel.【F:cmd/screener-core/main.go†L173-L325】
2. **Redis workers** – A configurable pool drains the channel, writes both raw and canonical hash keys via pipelined `HSET`s, and tracks per-exchange throughput counters and drop statistics.【F:cmd/screener-core/main.go†L395-L454】
3. **Health & metrics** – Background goroutines ping Redis, aggregate error counters, and periodically log rates (messages, Redis ops, drops) for observability.【F:cmd/screener-core/main.go†L463-L535】
4. **Shutdown** – POSIX signals close the supervision loop gracefully after flushing outstanding batches.【F:cmd/screener-core/main.go†L528-L535】

## 3. Exchange Connectors
- **Bybit**: Establishes a v5 public spot WebSocket, subscribes to `tickers.<symbol>`, parses `lastPrice`, and emits `MarketData` frames with timestamped prices while maintaining a keep-alive ping.【F:internal/exchange/bybit.go†L13-L107】【F:internal/exchange/bybit.go†L134-L149】
- **Gate.io**: Targets the v4 `spot.tickers` channel, converts Bybit symbols to Gate format, tolerates numeric or string payloads, and streams normalized quotes.【F:cmd/screener-core/main.go†L223-L265】【F:internal/exchange/gateio.go†L13-L142】
- **Bitget**: Supports batched subscriptions to respect the 10 msg/s cap, aggregates rate-limit error codes, and parses ticker payloads into canonical instrument identifiers.【F:cmd/screener-core/main.go†L267-L325】【F:internal/exchange/bitget.go†L15-L215】
- **OKX**: Uses batched `tickers` subscriptions, transforms symbols to OKX `instId` format, and handles error frames before forwarding parsed prices.【F:cmd/screener-core/main.go†L326-L388】【F:internal/exchange/okx.go†L13-L151】
- **Uniswap V2 (Ethereum)**: `internal/dex/Etherium/Uniswap/v2_connector.go` загружает перечень пулов из `ticker_source/geckoterminal_pools.json`, до запуска вызывает `AdjustPoolsOrdering` через HTTP RPC (нужно задать `http_url` в конфиге) чтобы выровнять фактический `token0/token1`, подписывается на `Sync` события и публикует цены в USD в общий поток так же, как CEX коннекторы.【F:internal/dex/Etherium/Uniswap/v2_connector.go†L41-L533】
- **Uniswap V3 (Ethereum)**: `internal/dex/Etherium/Uniswap/connect_uniswap_v3_all.go` читает список пулов из общего JSON (фильтрация по `gecko_dex`/`gecko_network`), подписывается на `Swap` события, лениво подтягивает метаданные токенов через HTTP RPC и публикует как сырой поток (`TOKEN0TOKEN1`), так и USD-деривацию через графовый прайсер.【F:internal/dex/Etherium/Uniswap/connect_uniswap_v3_all.go†L220-L934】【F:cmd/screener-core/main.go†L236-L309】
- **DEX USD-прайсер**: `internal/dex/pricing` собирает граф токенов на основе всех swap-событий (V2/V3/V4), хранит направленные рёбра с весами (цена, ликвидность) и по запросу строит короткий маршрут к стейблкоинам; результатом становятся точные `TOKENUSD` котировки через общий канал без каких-либо фиксированных 1.0.
- **DEX конфигурация**: Блок `dex_configs` в `configs/screener-core.yaml` описывает сеть, RPC/WS endpoints и перечень пулов; фабрика `launcher` передаёт `DexConfig` соответствующему билдеру (`uniswap_v2`, `uniswap_v3` и др.) и запускает его наряду с CEX-коннекторами через общий `LaunchContext`. Это устраняет ручные `switch` в `main.go` и унифицирует старт пайплайна.【F:configs/screener-core.yaml†L28-L63】【F:internal/launcher/register_dex.go†L1-L152】

## 4. Symbol Management
- **Loader**: `internal/util/symbol_loader.go` ingests GeckoTerminal-style JSON or legacy maps, drops stable-coin bases, and returns sorted uppercase tickers for further processing.【F:internal/util/symbol_loader.go†L11-L118】
- **Normalization helpers**: Utilities convert Bybit symbols to Gate format, strip separators for canonical keys, and attach default quotes when synthesizing subscription lists.【F:internal/util/helpers.go†L5-L64】

## 5. Redis Integration & Persistence
- **Client wrapper**: `internal/redisclient/client.go` wraps go-redis with structured logging, single-key helpers, and an `HSetBatch` pipeline used by workers to minimize round-trips.【F:internal/redisclient/client.go†L10-L105】
- **Storage layout**: Workers write to `price:<exchange>:<symbol>` (raw) and `price_canon:<canon>:<exchange>` (normalized) hash keys containing price, timestamp, exchange, and original symbol fields.【F:cmd/screener-core/main.go†L430-L439】

## 6. Logging & Diagnostics
- **Logger**: `internal/util/logger.go` initializes dual output to stdout and `screner.log`, supports dynamic log levels, and formats ISO-timestamped messages shared across packages.【F:internal/util/logger.go†L12-L76】
- **Metrics**: Throughput, Redis pipeline success, and drop counters are surfaced via periodic logs, aiding production dashboards and alerting.【F:cmd/screener-core/main.go†L493-L525】

## 7. Operations Tooling
- **Start/stop scripts**: `scripts/start_all.sh` builds the binary, ensures Redis availability (local or Docker Compose), and controls process PID files, while companion scripts manage status and shutdown flows.【F:scripts/start_all.sh†L1-L172】
- **Proto generation**: `scripts/generate_proto.sh` wraps `protoc` invocation to regenerate the Go bindings for the shared protobuf schema.【F:scripts/generate_proto.sh†L1-L16】

## 8. Extending the Platform
- **Adding exchanges**: Implement a connector mirroring the existing interface (`Connect`, `Subscribe`, `ReadLoop`, `KeepAlive`), register it in `main.go`, and supply symbol normalization helpers if the venue uses non-standard tickers.【F:internal/exchange/bybit.go†L20-L149】【F:cmd/screener-core/main.go†L173-L388】
- **Custom processors**: Downstream consumers can subscribe to Redis hash updates (raw or canonical) and derive arbitrage opportunities using the shared data contracts.【F:cmd/screener-core/main.go†L430-L439】【F:pkg/common/models.go†L3-L18】
