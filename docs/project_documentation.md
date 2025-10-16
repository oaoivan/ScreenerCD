# ScreenerCD Architecture Guide

## 1. Runtime Overview
- **Core service**: `cmd/screener-core/main.go` загружает YAML-конфигурацию, инициализирует Redis-клиент, настраивает логирование и запускает основной цикл обработки через `internal/launcher` (`Register/LaunchContext`). После запуска сервис блокирует выполнение `main.go` до сигнала. Логика по-прежнему остается внутри этого же inline-цикла.
- **Configuration schema**: `internal/config/config.go` определяет структуру Redis, параметры логирования и настройки бирж, включая списки активов для подписки (фьючерсы, спот, опционы Bitget), а также другие параметры.
- **Data structures**: Данные между сервисами передаются через Go-структуры (`pkg/common/models.go`) и protobuf-схемы (`pkg/protobuf/arbitrage.proto`), что обеспечивает кросс-языковую совместимость. `MarketData` несёт `exchange`, `symbol`, цену, timestamp, а также новые поля `network` и `chain_id` для различения сетей.

## 2. Data Flow Pipeline
1. **Exchange ingestion**: Коннекторы бирж подписываются на потоки данных, обрабатывают тикеры (например, парсят JSON) и отправляют нормализованные данные в общую шину (см. `cmd/screener-core/main.go`).
2. **Redis workers**: Горутины для каждой биржи, которые читают из канала и выполняют `HSET` с пакетированием в фоне для снижения накладных расходов/задержек.
3. **Health & metrics**: Сервис отслеживает состояние Redis, задержки сообщений и производительность, включая throughput (сообщений, байтов Redis, ошибок).
4. **Shutdown**: Обработка POSIX-сигналов для корректного завершения всех фоновых процессов и соединений.

## 3. Exchange Connectors
- **Bybit**: Использует WebSocket v5, подписывается на `tickers.<symbol>`, парсит `lastPrice`, формирует `MarketData` и поддерживает соединение с помощью keep-alive ping (см. `internal/exchange/bybit.go`).
- **Gate.io**: Работает с каналом v4 `spot.tickers`, унифицирует символы между Bybit и Gate, обрабатывает ошибки и распаковывает payload с использованием кастомного парсера (см. `internal/exchange/gateio.go`).
- **Bitget**: Обрабатывает пакетные обновления, лимит подписок в 10 сообщений/с, отслеживает rate-limit и решает ошибки с авторизацией (см. `internal/exchange/bitget.go`).
- **OKX**: Подписывается на канал `tickers`, сопоставляет символы с OKX `instId` и обрабатывает error-frames через кастомный код (см. `internal/exchange/okx.go`).
- **Uniswap V2 (Ethereum)**: Загружает список пар из `ticker_source/base_pools.json`, на лету вызывает `AdjustPoolsOrdering` по HTTP (через `http_url` в конфиге) для определения `token0/token1`, подписывается на событие `Sync` и публикует USD-цену в том же формате, что и CEX-коннекторы (см. `internal/dex/Etherium/Uniswap/v2_connector.go`).
- **Uniswap V3 (Ethereum)**: Также читает пулы из того же JSON (фильтр `gecko_dex`/`gecko_network`), слушает событие `Swap`, определяет `token0/token1` и `decimals` через on-chain `token0()/token1()`, что позволяет строить тикеры в формате `TOKEN0TOKEN1` + USD-эквивалент через граф цен (см. `internal/dex/Etherium/Uniswap/connect_uniswap_v3_all.go`).
- **Uniswap V4 (Base/Ethereum)**: Настраивается через `ticker_source/base_pools.json`, где указан `dex="uniswap_v4"` + адрес пула. WS/HTTP endpoints берутся из `ALCHEMY_API_KEY`, который загружается из `.env` при запуске `scripts/start_all.sh`; адрес PoolManager также берется из `dex_configs[].pool_manager`. Сервис поддерживает до 150 подписок, keep-alive каждые 25s; при ошибках соединения Alchemy использует экспоненциальный backoff (2s > 30s). Пары фильтруются по `WantedPairs`, как и в других коннекторах, и по белому списку в YAML (`wanted_pairs`, `wanted_pairs_only`).
- **DEX USD-прайсинг**: Модуль `internal/dex/pricing` строит граф цен на основе swap-событий (V2/V3/V4), находит оптимальный путь к стейблу (цена, ликвидность) и на его основе рассчитывает котировки в долларах, считая `TOKENUSD` тикером со значением 1.0.
- **MarketData & Pricing (DEX)**: protobuf `pkg/protobuf/arbitrage.proto` расширен полями `network` (строка) и `chain_id` (uint32). Коннекторы V2/V3/V4 заполняют эти поля при публикации `MarketData`, а `pkg/common.MarketData` держит синхронную копию. Графовый прайсер строит ключи вида `<dex_alias>:<network>` и связывает сети через мостовые токены (`TokenInfo.Bridge`).
- **DEX коннекторы**: Секция `dex_configs` в `configs/screener-core.yaml` определяет имя, RPC/WS endpoints и метаданные пулов; лаунчер `launcher` парсит `DexConfig` по типу коннектора (`uniswap_v2`, `uniswap_v3` и т.д.) и запускает его наравне с CEX-коннекторами через общий `LaunchContext`, заменяя `switch` в `main.go`.
	- Для V4 указываются `ws_url`, `http_url`, `pool_manager`, `max_meta_workers`, `subscribe_batch`, `ping_interval`; источник пулов может быть `pools_source` или `pools_file` (по умолчанию читается Gecko JSON). Для smoke-тестов можно указать `wanted_pairs_only=true`, чтобы подписаться только на указанные пары (например, `UNI/USDC`).

## 4. Symbol Management
- **Loader**: `internal/util/symbol_loader.go` загружает JSON GeckoTerminal для поиска пулов, фильтрует тикеры по белому списку и формирует нормализованные символы в едином формате.
- **Normalization helpers**: `internal/util/helpers.go` унифицирует форматы символов (Bybit > Gate), что необходимо для сопоставления цен и кросс-биржевой аналитики при арбитраже.

## 5. Redis Integration & Persistence
- **Client wrapper**: `internal/redisclient/client.go` абстрагирует go-redis, реализует асинхронный пайплайн, пакетную запись `HSetBatch`, а также отслеживает RTT.
- **Storage layout**: Текущая схема `price:<exchange>:<symbol>` (сырые) и `price_canon:<canon>:<exchange>` (нормализованные) хранит в HASH price, timestamp, exchange и другие метаданные. При переходе к мультисетям переходим на `price:<network>:<exchange>:<symbol>` и `price_canon:<network>:<canon>:<exchange>`; на время миграции допустимо дублировать запись в старый формат.

## 6. Logging & Diagnostics
- **Logger**: `internal/util/logger.go` пишет логи одновременно в stdout и `screner.log`, поддерживает структурированные поля и форматирует время в ISO-формате.
- **Logger**: `internal/util/logger.go` пишет логи одновременно в stdout и `screner.log`, поддерживает структурированные поля и форматирует время в ISO-формате. Для отключения вывода можно задать `SCR_LOG_STDOUT=false`, чтобы писать только в файл (упрощает отладку через `tail`).
- **Metrics**: Данные, полученные из Redis и других источников, агрегируются для отображения в дашборде.

## 7. Operations Tooling
- **Start/stop scripts**: `scripts/start_all.sh` запускает сервис, поднимает контейнер Redis (если он есть в Docker Compose) и сохраняет PID-файлы; `stop` скрипт убивает по PID и очищает. Для запуска используется переменная `.env`, которая передает `ALCHEMY_API_KEY` и `POOLMANAGER_V4` в сервис.
- **Proto generation**: `scripts/generate_proto.sh` использует `protoc` для генерации Go-кода из protobuf-схем. После обновления схемы с полями `network` и `chain_id` пересобираем всех потребителей, чтобы они читали расширенный `MarketData`.

## 8. Extending the Platform
- **Adding exchanges**: Реализуйте интерфейс коннектора (`Connect`, `Subscribe`, `ReadLoop`, `KeepAlive`), зарегистрируйте его в `internal/launcher`, и добавьте в `configs/assets/tokens.yaml`, чтобы тикеры и DEX-котировки имели общий базис. При сериализации новых коннекторов заполняем `MarketData.Network` и `MarketData.ChainID`, чтобы прайсер и Redis получали корректную метаинформацию.
- **Custom processors**: Модули-обработчики могут подписываться на Redis-каналы (raw или canonical) и строить аналитику, используя общие модели из `pkg/common`.
- **Multi-network DEX instances**: Добавление пулов для той же биржи/сети через несколько записей в `dex_configs`:
	1. **Конфиг**: для каждой записи указываем `network`, `network_id`, `chain_id`, `dex_alias`. Для источника пулов (`pools_source`) можно задать списки `dexes`, `networks` и карту `wanted_pairs_by_network`, чтобы фильтровать единый JSON.
		 ```yaml
		 dex_configs:
			 - name: "uniswap_v3"
				 dex_alias: "uniswap_v3"
				 network: "ethereum"
				 network_id: "ethereum"
				 chain_id: 1
				 pools_source:
					 file: "ticker_source/base_pools.json"
					 dexes: ["uniswap_v3"]
					 networks: ["ethereum"]
					 wanted_pairs_by_network:
						 ethereum: ["WETHUSDC"]
			 - name: "uniswap_v3"
				 dex_alias: "uniswap_v3"
				 network: "base"
				 network_id: "base-mainnet"
				 chain_id: 8453
				 pools_source:
					 file: "ticker_source/base_pools.json"
					 dexes: ["uniswap_v3"]
					 networks: ["base"]
		 ```
	2. **Assets**: обновляем `configs/assets/tokens.yaml`, добавляя блок сети (chain_id, native/stable токены). После правок прогоняем `go run ./test_scripts/assets/validate_tokens.go`.
	3. **TokenRegistry**: `internal/launcher` автоматически создаёт реестры с нужными alias/chain_id, но при кастомных алиасах можно дополнительно зарегистрировать их через `AssetsProvider.RegisterNetworkAlias`.
	4. **Пулы**: для новых сетей дополняем `ticker_source/base_pools.json`. Можно использовать вспомогательные скрипты из `test_scripts/uniswap` для проверки, что фильтры `dexes/networks` отбирают корректный поднабор.
	5. **Запуск**: каждый инстанс получает собственный supervisor-ярлык (`uniswap_v3:ethereum`, `uniswap_v3:base`) и отдельный Redis namespace (`price:<network>:<exchange>:<symbol>` после миграции на новый формат ключей).

## 9. Asset Registry
- Unified registry lives in `configs/assets/tokens.yaml`; connectors and pricers will pull stable/native metadata from here instead of hardcoded maps.
- Top-level keys follow `<network>_mainnet` naming and declare `chain_id` alongside the `stable` and `native` token arrays for that network.
- Each token entry defines `symbol`, `address`, and `decimals`; optional flags such as `wrapped: true` mark wrapped natives (e.g., WETH or WBNB).
- Extend coverage by adding a new network block or appending tokens to the existing lists while keeping checksum casing and on-chain decimals in sync.
- После правок `tokens.yaml` рекомендуется запустить валидатор, чтобы предотвратить skip-ошибки в рантайме. Скрипт валидации имеет два режима:

	```powershell
	go run ./test_scripts/assets/validate_tokens.go -dry-run
	go run ./test_scripts/assets/validate_tokens.go
	```

	Dry-run проверяет синтаксис, типы данных, и используется в CI. Второй вызов без параметров обновляет `stableSkipSet` и `isStablecoinOrFiat`.
