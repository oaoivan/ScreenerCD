# План рефакторинга фильтрации пулов по `base_pools.json`

## 1. Анализ текущего состояния
- Собрать фактические точки использования `ticker_source/base_pools.json` и идентифицировать, где фильтрация выполняется только по `amm_version`.
- Провести ревизию коннекторов Uniswap/PancakeSwap/других DEX: `internal/dex/Etherium/Uniswap/v2_connector.go`, `v3_connector`-ы, `v4_connector`, а также обертки в `internal/launcher` и `internal/processor`.
- Задокументировать текущую схему ключей и хранилищ в Redis, где наблюдается смешение пулов разных DEX.

## 2. Модель данных и контрактов
- Определить единый контракт сущности пула: `dex` + `amm_version` + `network` + идентификаторы токенов.
	- Структура `PoolDescriptor` и вспомогательные типы расположены в `internal/dex/pool_identity.go`, обеспечивают нормализацию ключа и валидацию обязательных полей.
- Обновить структуры данных в Go (модели, DTO), добавив отсутствующие поля или сделав их обязательными.
	- `internal/config/config.go`: `DexPoolConfig` теперь содержит `dex`, `amm_version`, `network`.
	- `internal/dex/Etherium/Uniswap/v2_connector.go`: `PoolConfig` хранит `Dex`, `AMMVersion`, `Network` и композитный ключ `CompositeKey`.
	- `internal/dex/Etherium/Uniswap/connect_uniswap_v3_all.go`: `v3PoolMeta` хранит идентификатор пула и валидируется через `PoolDescriptor`.
	- `internal/dex/Etherium/Uniswap/v4_connector.go`: `V4PoolConfig`/`PoolMeta` держат `Dex`, `AMMVersion`, `Network` и `CompositeKey`, inline/Gecko-пулы валидируются через `AttachDescriptor`.
- Обновить `base_pools.json`/loader так, чтобы при чтении формировался составной ключ и валидировались все три поля.
- Реализовано в `internal/dex/Etherium/Uniswap/v2_connector.go` (`LoadPoolsFromSourceWithOptions`) — фильтрация по `dex/amm/network`, уникальность `CompositeKey`.
- Добавить валидацию конфигурации: отклонять записи без одного из атрибутов или с конфликтами.
- Реализовано в `internal/config/config.go` (`validateInlineDexPools`) — проверка inline-пулов и детект дубликатов.

## 3. Переработка загрузки и фильтрации
- Обновить `internal/util/symbol_loader.go` и связанные сервисы, чтобы при подписке использовались все три атрибута.
- Реализовано в `internal/util/symbol_loader.go` (`LoadSymbolDescriptors`, фильтрация идентичностей) и `cmd/screener-core/main.go` (фильтр через `collectActiveSymbolIdentities`).
- В местах где происходит группировка по `amm_version`, заменить на группировку по `(dex, amm_version, network)`.
- Обновить логику формирования подписок в `cmd/*/main.go`, `internal/launcher/register_dex.go`, `internal/dex/.../pool_source_helpers.go`.
- Синхронизировать формат Redis-ключей, включая все три поля, и обеспечить миграцию существующих данных (см. план миграции в docs).

## 4. Коннекторы и пайплайны данных
- В Uniswap/PancakeSwap коннекторах гарантировать, что фильтрация/регистрация пулов учитывает `dex` и `network` вместе с `amm_version`.
- Для v2/v3/v4 реализовать явное сопоставление DEX ↔ сетей: разные клиенты/ABI/эндпоинты для каждой комбинации.
- Проверить подписку на BSC, чтобы исключить попадание чужих пулов в пространство Uniswap.

## 5. Тестирование
- Дописать юнит-тесты и интеграционные тесты для загрузчиков/коннекторов, проверяющие отсутствие перекрестного попадания пулов.
- Добавить тестовые конфиги с конфликтными данными и убедиться, что система их отвергает.
- Прогнать e2e-тесты подписки/публикации, убедиться в корректности хранения в Redis.

## 6. Миграция и развертывание
- Подготовить скрипт очистки/миграции ключей Redis, где ключи не соответствуют новой схеме.
- Обновить документацию (`docs/base_pools_migration_plan.md`, README) о новой схеме фильтрации.
- Спланировать поэтапный rollout: выкладка обновленных сервисов, миграция данных, мониторинг аномалий.

## 7. Мониторинг и поддержка
- Включить логирование тройки `(dex, amm_version, network)` при подписках и при записи в Redis.
- Добавить метрики/алерты на обнаружение чужих DEX внутри пространства Uniswap или отсутствия данных по v3/v4.
- Провести пост-рефакторинговый аудит конфигураций, чтобы избежать повторения проблемы.


Анализ шага 1

v2_connector.go (LoadPoolsFromSource*, PoolSourceOptions): читает base_pools.json, фильтрует только AMMVersions и NetworkFilters; DexFilters вообще не применяются (кроме хардкодного fallback).
register_dex.go (buildUniswapConfig, buildUniswapV3Config, buildUniswapV4Config): формирует PoolSourceOptions, но не заполняет DexFilters, из-за чего loader не знает про выбранный DEX.
connect_uniswap_v3_all.go (v3Engine.loadPools) и v4_connector.go (V4Connector.loadPoolsFromFile): тоже используют base_pools.json, фильтруют по amm_version/network, при этом поле dex из JSON полностью игнорируется.
Вспомогательные функции dexMatchesAny / normalizeDexName в pool_source_helpers.go есть, но основной поток загрузки их не вызывает.
Конфигурации (screener-core.yaml) и тесты (test_scripts/*) указывают на тот же файл и попадают под текущие ограничения фильтрации.
Текущая схема Redis в main.go и processor.go: ключи price:{network}:{exchange}:{symbol} и price_canon:{network}:{canon}:{exchange}. В ключе нет dex/amm_version; если коннектор выдаёт чужой пул, он оказывается в пространстве uniswap_v2 той же сети, что подтверждает проблему смешения.