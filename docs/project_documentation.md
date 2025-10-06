# ScreenerCD Architecture Guide

## 1. Runtime Overview
- **Core service**: `cmd/screener-core/main.go` загружает YAML-конфигурацию, поднимает Redis-клиент, определяет включённые площадки и делегирует запуск коннекторов фабрике `internal/launcher` (`Register/LaunchContext`). Чтобы добавить биржу, достаточно зарегистрировать новый билдер — изменения `main.go` не нужны. Символьные списки по-прежнему собираются через общие пулы и inline-конфиг.
- **Configuration schema**: `internal/config/config.go` описывает параметры Redis, источники символов и настройки батчинга, задавая безопасные значения по умолчанию (буферы, воркеры, лимиты Bitget), когда опции пропущены.
- **Data structures**: Снимки рынка описаны одновременно Go-структурами (`pkg/common/models.go`) и protobuf-схемой (`pkg/protobuf/arbitrage.proto`), что обеспечивает единообразную сериализацию.

## 2. Data Flow Pipeline
1. **Exchange ingestion**: супервизоры поднимают коннектор на каждую площадку, подписываются батчами (учитывая лимиты бирж) и складывают нормализованные тикеры в общий буфер (см. `cmd/screener-core/main.go`).
2. **Redis workers**: настраиваемый пул забирает данные из канала, пишет сырой и канонический `HSET` в пайплайне и ведёт счётчики пропускной способности/дропов.
3. **Health & metrics**: фоновые горутины пингуют Redis, агрегируют ошибки и периодически логируют throughput (сообщения, операции Redis, дропы).
4. **Shutdown**: обработка POSIX-сигналов закрывает цикл супервайзеров после сброса оставшихся батчей.

## 3. Exchange Connectors
- **Bybit**: Открывает публичный WebSocket v5, подписывается на `tickers.<symbol>`, парсит `lastPrice`, эмитит `MarketData` с таймстемпом и поддерживает keep-alive ping (см. `internal/exchange/bybit.go`).
- **Gate.io**: Работает с каналом v4 `spot.tickers`, преобразует символы из формата Bybit в Gate, принимает числовые и строковые payload и публикует нормализованные котировки (см. `internal/exchange/gateio.go`).
- **Bitget**: Формирует пакетные подписки, чтобы уложиться в лимит 10 сообщений/с, аггрегирует ошибки rate-limit и приводит тикеры к каноническим идентификаторам (см. `internal/exchange/bitget.go`).
- **OKX**: Использует пакетные подписки `tickers`, трансформирует символы в формат OKX `instId` и фильтрует error-frames перед выдачей цен (см. `internal/exchange/okx.go`).
- **Uniswap V2 (Ethereum)**: Коннектор загружает пулы из `ticker_source/geckoterminal_pools.json`, до старта дергает `AdjustPoolsOrdering` по HTTP (нужен `http_url` в конфиге) для выравнивания `token0/token1`, подписывается на события `Sync` и публикует USD-цены в общий поток так же, как CEX-коннекторы (см. `internal/dex/Etherium/Uniswap/v2_connector.go`).
- **Uniswap V3 (Ethereum)**: Читает список пулов из общего JSON (фильтры `gecko_dex`/`gecko_network`), слушает события `Swap`, сверяет `token0/token1` и `decimals` через on-chain `token0()/token1()`, при необходимости меняет порядок и отдает нормализованные котировки `TOKEN0TOKEN1` + USD-деривацию через графовый прайсер (см. `internal/dex/Etherium/Uniswap/connect_uniswap_v3_all.go`).
- **Uniswap V4 (Base/Ethereum)**: Использует тот же `ticker_source/geckoterminal_pools.json`, фильтруя записи `dex="uniswap_v4"` + нужные пары. WS/HTTP endpoints строятся из `ALCHEMY_API_KEY`, который подхватывается из `.env` при запуске `scripts/start_all.sh`; адрес PoolManager задаётся через `POOLMANAGER_V4` (см. `dex_configs[].pool_manager`). Батчи подписки ограничены 150 пулами, keep-alive каждые 25s; при превышении лимитов Alchemy действует экспоненциальный backoff (2s → 30s). Для запуска нужны те же `WantedPairs`, что у тестового скрипта — их можно задать в YAML (`wanted_pairs`, `wanted_pairs_only`).
- **DEX USD-прайсер**: Пакет `internal/dex/pricing` строит граф токенов по всем swap-событиям (V2/V3/V4), хранит направленные ребра с весами (цена, ликвидность) и на запрос выдает кратчайший маршрут к стейблам, формируя точные `TOKENUSD` котировки без забитых 1.0.
- **DEX конфигурация**: Блок `dex_configs` в `configs/screener-core.yaml` описывает сеть, RPC/WS endpoints и перечень пулов; фабрика `launcher` передает `DexConfig` нужному билдеру (`uniswap_v2`, `uniswap_v3` и др.) и запускает их рядом с CEX-коннекторами через общий `LaunchContext`, устраняя ручные `switch` в `main.go`.
	- Для V4 обязательны поля `ws_url`, `http_url`, `pool_manager`, `max_meta_workers`, `subscribe_batch`, `ping_interval`; источник пулов задаём через `pools_source` или `pools_file` (по умолчанию общий Gecko JSON). При smoke-тестах можно включить `wanted_pairs_only=true`, чтобы подписаться только на ограниченный список (например, `UNI/USDC`).

## 4. Symbol Management
- **Loader**: `internal/util/symbol_loader.go` загружает JSON GeckoTerminal или легаси карты, выкидывает стейблы из базовой валюты и возвращает отсортированные тикеры в верхнем регистре.
- **Normalization helpers**: `internal/util/helpers.go` конвертирует символы между форматами (Bybit → Gate), убирает разделители для канонических ключей и подставляет дефолтные котировки при генерации подписок.

## 5. Redis Integration & Persistence
- **Client wrapper**: `internal/redisclient/client.go` оборачивает go-redis, добавляя структурированные логи, хелперы для одиночных ключей и пайплайн `HSetBatch`, чтобы рабочие минимизировали RTT.
- **Storage layout**: Воркеры пишут `price:<exchange>:<symbol>` (сырой) и `price_canon:<canon>:<exchange>` (нормализованный) хеши с полями price, timestamp, exchange и оригинальный символ.

## 6. Logging & Diagnostics
- **Logger**: `internal/util/logger.go` ведёт вывод одновременно в stdout и `screner.log`, поддерживает динамические уровни и форматирует сообщения с ISO-временем.
- **Metrics**: Трафик, успешность пайплайна Redis и счётчики дропов выводятся периодическими логами для мониторинга и алёртов.

## 7. Operations Tooling
- **Start/stop scripts**: `scripts/start_all.sh` собирает бинарь, проверяет наличие Redis (локально или через Docker Compose) и управляет PID-файлами; соседние скрипты отвечают за статус и остановку. При старте автоматически подхватывает `.env`, поэтому достаточно обновить `ALCHEMY_API_KEY` и `POOLMANAGER_V4` в одном месте.
- **Proto generation**: `scripts/generate_proto.sh` оборачивает вызов `protoc` для регенерации Go-байндингов общей protobuf-схемы.

## 8. Extending the Platform
- **Adding exchanges**: Реализуйте коннектор с интерфейсом (`Connect`, `Subscribe`, `ReadLoop`, `KeepAlive`), зарегистрируйте билдер в `internal/launcher`, и расширьте `configs/assets/tokens.yaml`, чтобы прайсер и DEX-коннекторы получили свежие якоря.
- **Custom processors**: Даунстрим-потребители могут подписываться на Redis-хеши (raw или canonical) и собирать арбитраж, используя общие модели из `pkg/common`.

## 9. Asset Registry
- Unified registry lives in `configs/assets/tokens.yaml`; connectors and pricers will pull stable/native metadata from here instead of hardcoded maps.
- Top-level keys follow `<network>_mainnet` naming and declare `chain_id` alongside the `stable` and `native` token arrays for that network.
- Each token entry defines `symbol`, `address`, and `decimals`; optional flags such as `wrapped: true` mark wrapped natives (e.g., WETH or WBNB).
- Extend coverage by adding a new network block or appending tokens to the existing lists while keeping checksum casing and on-chain decimals in sync.
- После любого изменения `tokens.yaml` прогоняйте валидатор, чтобы синхронизировать skip-листы и проверить данные. Сначала запускаем проверку без изменений, затем — полный прогон:

	```powershell
	go run ./test_scripts/assets/validate_tokens.go -dry-run
	go run ./test_scripts/assets/validate_tokens.go
	```

	Dry-run завершается ошибкой, если требуются правки, и используется в CI. Полный запуск при необходимости дописывает новые стейблы в `stableSkipSet` и `isStablecoinOrFiat`.
