# PancakeSwap V2 Connector Delivery Plan

## 1. Discovery & Prerequisites
- [ ] Проанализировать пайплайн ScreenerCD: входящие данные, Redis-воркеры и фабрику коннекторов, чтобы убедиться в совместимости интерфейсов (`LaunchContext`, модели `MarketData`).
- [ ] Собрать инфо о сети BNB Chain: RPC/WS эндпоинты, адрес Factory/Router, ABI для событий `Swap`/`Sync`, параметры блоков и лимиты запросов.
- [ ] Перечислить необходимые токены/стейблы в `configs/assets/tokens.yaml`, добавить метаданные для BNB Chain при отсутствии.

## 2. Источник пулов и конфигурация
- [ ] Определить источник пулов PancakeSwap V2 (GeckoTerminal API, собственный JSON). Сформировать `ticker_source/pancakeswap_pools.json` по аналогии с существующими файлами.
- [ ] Обновить `configs/screener-core.yaml`: добавить `dex_configs` для PancakeSwap V2 (BNB Chain) с `ws_url`, `http_url`, `pools_source/pools_file`, `wanted_pairs`, батчами подписок и keep-alive.
- [ ] Проверить, что `.env` содержит ключи (например, BSC RPC/WS); обновить `scripts/start_all.sh`, если требуется новая переменная окружения.

## 3. Реализация коннектора
- [ ] Создать модуль `internal/dex/Bnb/Pancakeswap/v2_connector.go`, придерживаясь паттернов существующих Uniswap-коннекторов.
- [ ] Реализовать логику подписки на события `Swap`/`Sync` через WebSocket или RPC, аггрегируя данные в общий буфер `MarketData`.
- [ ] Обеспечить пересчёт цен в USD через имеющийся прайсер графа (`internal/dex/pricing`).
- [ ] Обработать порядок `token0/token1`, кэшировать decimals, внедрить backoff/keep-alive, логирование в стиле проекта.

## 4. Интеграция в пайплайн
- [ ] Зарегистрировать билдер в `internal/launcher/register_dex.go`, чтобы коннектор стартовал из `cmd/screener-core` без ручных правок.
- [ ] Обновить `internal/util/symbol_loader.go` при необходимости новых фильтров для PancakeSwap.
- [ ] Гарантировать совместимость с Redis-воркерами: формировать канонические ключи, использовать `HSetBatch` (через существующие клиенты).

## 5. Тестирование
- [ ] Написать unit/integration тесты (по образцу `internal/dex/Etherium/Uniswap/v4_connector_test.go`) для проверки парсинга событий и нормализации цен.
- [ ] Добавить smoke-скрипт (`test_scripts/uniswap/pancakeswap/connect_pancakeswap_v2.go`) для локальной проверки подключения, лимитов и корректности данных.
- [ ] Обновить документацию с инструкцией по запуску и особенностям сети.

## 6. Роллаут
- [ ] Прогнать `go test ./...` и специализированные скрипты перед деплоем.
- [ ] Обновить `docs/project_documentation.md`/`base_pools_migration_plan.md` разделами про PancakeSwap V2.
- [ ] Подготовить чеклист запуска: настройка .env, старт сервисов, мониторинг логов/Redis ключей, проверка USD-котировок.
