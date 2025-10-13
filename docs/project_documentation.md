# ScreenerCD Architecture Guide

## 1. Runtime Overview
- **Core service**: `cmd/screener-core/main.go` ��������� YAML-������������, ��������� Redis-������, ���������� ���������� �������� � ���������� ������ ����������� ������� `internal/launcher` (`Register/LaunchContext`). ����� �������� �����, ���������� ���������������� ����� ������ � ��������� `main.go` �� �����. ���������� ������ ��-�������� ���������� ����� ����� ���� � inline-������.
- **Configuration schema**: `internal/config/config.go` ��������� ��������� Redis, ��������� �������� � ��������� ��������, ������� ���������� �������� �� ��������� (������, �������, ������ Bitget), ����� ����� ���������.
- **Data structures**: ������ ����� ������� ������������ Go-����������� (`pkg/common/models.go`) � protobuf-������ (`pkg/protobuf/arbitrage.proto`), ��� ������������ ������������� ������������. `MarketData` несёт `exchange`, `symbol`, цену, timestamp, а также новые поля `network` и `chain_id` для различения сетей.

## 2. Data Flow Pipeline
1. **Exchange ingestion**: ����������� ��������� ��������� �� ������ ��������, ������������� ������� (�������� ������ ����) � ���������� ��������������� ������ � ����� ����� (��. `cmd/screener-core/main.go`).
2. **Redis workers**: ������������� ��� �������� ������ �� ������, ����� ����� � ������������ `HSET` � ��������� � ���� �������� ���������� �����������/������.
3. **Health & metrics**: ������� �������� ������� Redis, ���������� ������ � ������������ �������� throughput (���������, �������� Redis, �����).
4. **Shutdown**: ��������� POSIX-�������� ��������� ���� ������������� ����� ������ ���������� ������.

## 3. Exchange Connectors
- **Bybit**: ��������� ��������� WebSocket v5, ������������� �� `tickers.<symbol>`, ������ `lastPrice`, ������ `MarketData` � ����������� � ������������ keep-alive ping (��. `internal/exchange/bybit.go`).
- **Gate.io**: �������� � ������� v4 `spot.tickers`, ����������� ������� �� ������� Bybit � Gate, ��������� �������� � ��������� payload � ��������� ��������������� ��������� (��. `internal/exchange/gateio.go`).
- **Bitget**: ��������� �������� ��������, ����� ��������� � ����� 10 ���������/�, ����������� ������ rate-limit � �������� ������ � ������������ ��������������� (��. `internal/exchange/bitget.go`).
- **OKX**: ���������� �������� �������� `tickers`, �������������� ������� � ������ OKX `instId` � ��������� error-frames ����� ������� ��� (��. `internal/exchange/okx.go`).
- **Uniswap V2 (Ethereum)**: ��������� ��������� ���� �� `ticker_source/base_pools.json`, �� ������ ������� `AdjustPoolsOrdering` �� HTTP (����� `http_url` � �������) ��� ������������ `token0/token1`, ������������� �� ������� `Sync` � ��������� USD-���� � ����� ����� ��� ��, ��� CEX-���������� (��. `internal/dex/Etherium/Uniswap/v2_connector.go`).
- **Uniswap V3 (Ethereum)**: ������ ������ ����� �� ������ JSON (������� `gecko_dex`/`gecko_network`), ������� ������� `Swap`, ������� `token0/token1` � `decimals` ����� on-chain `token0()/token1()`, ��� ������������� ������ ������� � ������ ��������������� ��������� `TOKEN0TOKEN1` + USD-��������� ����� �������� ������� (��. `internal/dex/Etherium/Uniswap/connect_uniswap_v3_all.go`).
- **Uniswap V4 (Base/Ethereum)**: ���������� ��� �� `ticker_source/base_pools.json`, �������� ������ `dex="uniswap_v4"` + ������ ����. WS/HTTP endpoints �������� �� `ALCHEMY_API_KEY`, ������� �������������� �� `.env` ��� ������� `scripts/start_all.sh`; ����� PoolManager ������� ����� `POOLMANAGER_V4` (��. `dex_configs[].pool_manager`). ����� �������� ���������� 150 ������, keep-alive ������ 25s; ��� ���������� ������� Alchemy ��������� ���������������� backoff (2s > 30s). ��� ������� ����� �� �� `WantedPairs`, ��� � ��������� ������� � �� ����� ������ � YAML (`wanted_pairs`, `wanted_pairs_only`).
- **DEX USD-�������**: ����� `internal/dex/pricing` ������ ���� ������� �� ���� swap-�������� (V2/V3/V4), ������ ������������ ����� � ������ (����, �����������) � �� ������ ������ ���������� ������� � ��������, �������� ������ `TOKENUSD` ��������� ��� ������� 1.0.
- **MarketData & Pricing (DEX)**: protobuf `pkg/protobuf/arbitrage.proto` ���������� ���� `network` (����� ����) � `chain_id` (uint32). Коннекторы V2/V3/V4 ������������� ��� поля при публикации `MarketData`, а `pkg/common.MarketData` держит синхронную копию. Графовый прайсер строит ключи вида `<dex_alias>:<network>` и связывает сети через мостовые токены (`TokenInfo.Bridge`).
- **DEX ������������**: ���� `dex_configs` � `configs/screener-core.yaml` ��������� ����, RPC/WS endpoints � �������� �����; ������� `launcher` �������� `DexConfig` ������� ������� (`uniswap_v2`, `uniswap_v3` � ��.) � ��������� �� ����� � CEX-������������ ����� ����� `LaunchContext`, �������� ������ `switch` � `main.go`.
	- ��� V4 ����������� ���� `ws_url`, `http_url`, `pool_manager`, `max_meta_workers`, `subscribe_batch`, `ping_interval`; �������� ����� ����� ����� `pools_source` ��� `pools_file` (�� ��������� ����� Gecko JSON). ��� smoke-������ ����� �������� `wanted_pairs_only=true`, ����� ����������� ������ �� ������������ ������ (��������, `UNI/USDC`).

## 4. Symbol Management
- **Loader**: `internal/util/symbol_loader.go` ��������� JSON GeckoTerminal ��� ������ �����, ���������� ������� �� ������� ������ � ���������� ��������������� ������ � ������� ��������.
- **Normalization helpers**: `internal/util/helpers.go` ������������ ������� ����� ��������� (Bybit > Gate), ������� ����������� ��� ������������ ������ � ����������� ��������� ��������� ��� ��������� ��������.

## 5. Redis Integration & Persistence
- **Client wrapper**: `internal/redisclient/client.go` ����������� go-redis, �������� ����������������� ����, ������� ��� ��������� ������ � �������� `HSetBatch`, ����� ������� �������������� RTT.
- **Storage layout**: ������� ����� `price:<exchange>:<symbol>` (�����) � `price_canon:<canon>:<exchange>` (���������������) ���� � ������ price, timestamp, exchange � ������������ ������. При переходе к мультисетям переходим на `price:<network>:<exchange>:<symbol>` и `price_canon:<network>:<canon>:<exchange>`; на время миграции допустимо дублировать запись в старый формат.

## 6. Logging & Diagnostics
- **Logger**: `internal/util/logger.go` ���� ����� ������������ � stdout � `screner.log`, ������������ ������������ ������ � ����������� ��������� � ISO-��������.
- **Logger**: `internal/util/logger.go` ���� ����� ������������ � stdout � `screner.log`, ������������ ������������ ������ � ����������� ��������� � ISO-��������. ��� ������� ����� ������� ����� ������ `SCR_LOG_STDOUT=false`, ����� �������� ������ ������ � ���� (�������� ������������ ����� ��� ��������� stdout).
- **Metrics**: ������, ���������� ��������� Redis � �������� ������ ��������� �������������� ������ ��� ����������� � ������.

## 7. Operations Tooling
- **Start/stop scripts**: `scripts/start_all.sh` �������� ������, ��������� ������� Redis (�������� ��� ����� Docker Compose) � ��������� PID-�������; �������� ������� �������� �� ������ � ���������. ��� ������ ������������� ������������ `.env`, ������� ���������� �������� `ALCHEMY_API_KEY` � `POOLMANAGER_V4` � ����� �����.
- **Proto generation**: `scripts/generate_proto.sh` ����������� ����� `protoc` ��� ����������� Go-���������� ����� protobuf-�����. После обновления схемы с полями `network` и `chain_id` пересобираем всех потребителей, чтобы они читали расширенный `MarketData`.

## 8. Extending the Platform
- **Adding exchanges**: ���������� ��������� � ����������� (`Connect`, `Subscribe`, `ReadLoop`, `KeepAlive`), ��������������� ������ � `internal/launcher`, � ��������� `configs/assets/tokens.yaml`, ����� ������� � DEX-���������� �������� ������ �����. При сериализации новых коннекторов заполняем `MarketData.Network` и `MarketData.ChainID`, чтобы прайсер и Redis получали корректную метаинформацию.
- **Custom processors**: ���������-����������� ����� ������������� �� Redis-���� (raw ��� canonical) � �������� ��������, ��������� ����� ������ �� `pkg/common`.
- **Multi-network DEX instances**: ���������� ���� ���������� ��� ���� �������/сети ������� ����������� ������ ��� `dex_configs`:
	1. **Конфиг**: ��� каждой записи указываем `network`, `network_id`, `chain_id`, `dex_alias`. Для источника пулов (`pools_source`) можно задать списки `dexes`, `networks` и карту `wanted_pairs_by_network`, чтобы фильтровать единый JSON.
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
- ����� ������ ��������� `tokens.yaml` ���������� ���������, ����� ���������������� skip-����� � ��������� ������. ������� ��������� �������� ��� ���������, ����� � ������ ������:

	```powershell
	go run ./test_scripts/assets/validate_tokens.go -dry-run
	go run ./test_scripts/assets/validate_tokens.go
	```

	Dry-run ����������� �������, ���� ��������� ������, � ������������ � CI. ������ ������ ��� ������������� ���������� ����� ������� � `stableSkipSet` � `isStablecoinOrFiat`.
