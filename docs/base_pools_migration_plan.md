# План перехода на `ticker_source/base_pools.json`

## 1. Контекст и цель
- Проект Screner устроен вокруг пайплайна `cmd/screener-core` → подписки через `internal/launcher` → DEX/CEX коннекторы (`internal/dex`, `internal/exchange`) → публикация в Redis (`internal/redisclient`) → окно пользователя.
- Сейчас все DEX-коннекторы и генерация списков тикеров опираются на `ticker_source/base_pools_pools.json`, что просачивается в код (`internal/dex/Etherium/Uniswap/*`, `internal/util/symbol_loader.go`, `configs/screener-core.yaml`) и документацию.
 - Файл `configs/screener-core.yaml` переключить на `ticker_source/base_pools.json` с фильтрацией `dex_filter`/`network_filter`/`amm_version`.
- Цель: единый источник `ticker_source/base_pools.json`, в котором критичны поля `dex`, `amm_version`, `network`. Суффиксы наподобие `_v3` в `dex` игнорируем, версию берем только из `amm_version`.

## 2. Актуальные зависимости от Gecko-файла
- Конфигурация: `configs/screener-core.yaml` (`shared_pools`, `dex_configs[].pools_source`), `internal/config.PoolsSource` с полями `DexFilter/NetworkFilter`.
 - Файл `configs/screener-core.yaml` переключить на `ticker_source/base_pools.json` с фильтрацией `dex_filter`/`network_filter`/`amm_version`.
 - В `cmd/screener-core` использовать `dex_filter`/`network_filter`/`amm_version` и `symbols_only` для загрузки тикеров из base_pools.
- Загрузка базовых символов CEX: `internal/util/symbol_loader.go` (парсер `base_poolsFile`).
- Uniswap V2: `internal/dex/Etherium/Uniswap/v2_connector.go` (`LoadPoolsFromBase`, `GeckoPool`-структуры).
- Uniswap V3: `internal/dex/Etherium/Uniswap/connect_uniswap_v3_all.go` (`v3GeckoPool`, фильтры по `dex_filter`, `network_filter`).
- Uniswap V4: `internal/dex/Etherium/Uniswap/v4_connector.go` (`GeckoEntry`, `defaultPoolsPath` и т.д.).
- Launcher и режимы подстановки: `internal/launcher/register_dex.go`.
- Документация и скрипты (`docs/project_documentation.md`, `docs/uniswap_v4_integration_plan.md`, `test_scripts/*`, `Temp/*.txt`) прямо указывают на `base_pools_pools.json`.

## 3. Требования к новому источнику
- Используем `entries[].dex`, `entries[].amm_version`, `entries[].network` для фильтров, не полагаемся на имя файла или суффиксы `dex` вида `_v3`.
- Поддерживаем существующие поля пулов (`pool_address`, `token{0,1}`, `pool_key`, `pool_manager`), так как коннекторы строят подписки и расшифровывают события.
- Обеспечиваем единый слой нормализации `dex` (strip суффиксы) и валидации `amm_version` (`v1/v2/v3/v4/...`).
- Совместимость: публичные структуры `uniswap.PoolConfig`, `V3Config`, `V4Config` не меняем резко; миграция должна быть ступенчатой.

## 4. Пошаговый план

### Этап 0. Подготовка и проверка данных
- Провести sanity-check `ticker_source/base_pools.json`: валидный JSON, наличие ключевых полей, объём (890 записей) укладывается в рабочие лимиты.
- Сформировать выборку тестовых пулов для разных `amm_version` (v1/v2/v3/v4) и сетей (ethereum, base, avalanche, linea и т.д.) — понадобится для модульных тестов.
- Зафиксировано: JSON успешно распарсен, сеть BSC обозначена как `binance-smart-chain`. Подготовлен тестовый срез `testdata/base_pools_samples.json` (ethereum v2/v3/v4 + binance-smart-chain v2/v3/v4). Для BSC подписки используем Alchemy endpoint `https://bnb-mainnet.g.alchemy.com/v2/${ALCHEMY_API_KEY}` (ключ тот же, что и для ETH).

### Этап 1. Общий слой модели base_pools
- Вынести описание структуры в новую единицу (например, `internal/pools/base` или `pkg/basepools`): структуры `Entry`, `Token`, `PoolKey`.
- Добавлены хелперы `NormalizeDex`, `ParseAMMVersion`, `Entry.Matches` в `internal/pools/base`, чтобы унифицировать фильтрацию по `dex`/`amm_version`/`network`.
- Реализован загрузчик `basepools.LoadBasePools` с кешированием по пути, чтобы коннекторы могли переиспользовать общий список без лишних IO.
- Добавлены адаптеры `ToUniswapV2Pool`, `ToUniswapV3Pool`, `ToUniswapV4Pool` в `internal/pools/base` плюс тесты на срезе `testdata/base_pools_samples.json`.
- Добавить хелперы:
  - `NormalizeDex(string) string` — удаление суффиксов `_v\d+`, `_amm`, перевод в нижний регистр.
  - `ParseAMMVersion(string) (Version, error)` — контролируемый enum/строка с fallback `unknown`.
  - `Matches(filter)` — предикат по `dex`, `amm_version`, `network`, опционально по `symbol`.
- Реализовать загрузчик `LoadBasePools(path string) ([]Entry, error)` с кэшированием по пути (аналогично `util.LoadSymbolsFromFile`).
- Реализовать адаптеры для текущих коннекторов:
  - `ToUniswapV2Pool(entry)` возвращает `uniswap.PoolConfig`.
  - `ToUniswapV3Pool(entry)` готовит структуру для подписки (включая `PoolID`, `token decimals`).
  - `ToUniswapV4Pool(entry)` отдает `V4PoolConfig`, проверяя `pool_manager`.

### Этап 2. Изменения конфигурации
- В `internal/config.PoolsSource` заменить `DexFilter/NetworkFilter` на нейтральные `DexFilter`, `NetworkFilter`, добавить `AmmVersionFilter`, `SymbolsOnly` (если нужно ограничить список для CEX).
 - В `cmd/screener-core` использовать `dex_filter`/`network_filter`/`amm_version` и `symbols_only` для загрузки тикеров из base_pools.
- Обновить `ResolveSharedPoolsPath` и `DexConfig.ResolvePoolsPath`, чтобы они не зависели от старого названия.
 - Файлы `ResolveSharedPoolsPath` и `DexConfig.ResolvePoolsPath` делают fallback на `ticker_source/base_pools.json` вместо логики с Gecko.
- Переписать валидацию `DexConfig.Validate()` — проверка, что при использовании источника base_pools задан `DexFilter` и `AmmVersionFilter` (где требуется).
- Обновить `configs/screener-core.yaml`: все ссылки меняем на `ticker_source/base_pools.json`, обновляем ключи `pools_source` на новые имя полей.
 - Файл `configs/screener-core.yaml` переключить на `ticker_source/base_pools.json` с фильтрацией `dex_filter`/`network_filter`/`amm_version`.
- Пересмотреть переменные окружения (`GECKO_POOLS_JSON` → `BASE_POOLS_JSON`), описать fallback-поведение.

### Этап 3. Обновление загрузчика символов (CEX)
- Переписать `internal/util/symbol_loader.go`:
  - Использовать новый `basepools.Entry`.
  - Ввести флаг (параметр или env) для фильтрации символов, например `include_spot_only` (опционально, пока достаточно фильтра `amm_version` ∈ {`v1`,`v2`,`v3`,`v4`}).
  - Сохраняем `stableSkipSet`, но опираемся на `entry.symbol`.
- Обновить вызовы `LoadSymbolsFromFile` в `cmd/screener-core/main.go`, убедиться, что дефицит полей не приводит к панике.
- Добавить модульные тесты на новые кейсы (duplicates, пустые символы, stable-строки).

### Этап 4. Uniswap V2 коннектор
- Заменить `LoadPoolsFromBase*` на `LoadPoolsFromBase`:
  - Подгружаем все `basepools.Entry`.
  - Фильтруем по `NormalizeDex(entry.dex) == "uniswap"` и `amm_version == "v2"` (или `dex_cfg.name`, если поддерживаем кастомные DEX).
  - Для каждой записи формируем `PoolConfig` (address, tokens, decimals, base token ориентация).
- Очистить код от `GeckoPool`/`geckoIntOrText`.
- Пересмотреть обработку полей `BaseIsToken0`, `CanonicalPair`, `StableSymbol` — при необходимости добавить логику на базе `basepools.Entry`.
- Обновить логику построения логов и ошибок (сообщения больше не упоминают base_pools).

### Этап 5. Uniswap V3 коннектор
- Перейти на новый парсер:
  - Фильтр: `NormalizeDex(entry.dex) == "uniswap"` и `amm_version == "v3"`, `entry.network == dexCfg.Network`.
  - Убедиться, что `pool_id` и `pool_address` заполняются (если отсутствуют — лог ошибки и пропуск).
- Переписать структуры `v3GeckoPool`, `v3GeckoFile` → использовать `basepools.Entry`.
- Перепроверить расчёт `SubscribeBatch` и очередность подписок — данные `pool_key` (fee, tickSpacing) всё ещё доступны.
- Адаптировать код, который подтягивает decimals через RPC: если `entry.token0.decimals` уже валиден, уменьшить число вызовов.

### Этап 6. Uniswap V4 коннектор
- Заменить `defaultPoolsPath` и весь парсер Gecko на новую модель.
- Фильтр: `NormalizeDex(entry.dex) == "uniswap"` и `amm_version == "v4"`, `network` из конфига, проверка `pool_manager`.
- Убедиться, что `pool_key.hooks`, `pool_key.tickSpacing`, `pool_manager` читаются из `base_pools`.
- Поддержать кастомные пулы в `dexCfg.Pools` (inline) без регресса.
- Обновить логи/метрики: при ошибке указывать `base_pools` и `entry.pool_address`.

### Этап 7. Launcher и общая инициализация
- В `internal/launcher/register_dex.go` обновить вызовы: `LoadPoolsFromBaseWithRegistry`.
- Передавать в загрузчик фильтры из `DexConfig.PoolsSource` (`DexFilter`, `AmmVersionFilter`, `NetworkFilter`).
- Отразить изменения в логах запуска (`util.Infof`), чтобы можно было проверить какой источник и сколько пулов использовано.

### Этап 8. Прочие потребители
- Просканировать `test_scripts`, `Temp`, `scripts` на вхождения `base_pools_pools.json` и обновить пути.
- Проверить `internal/dex/pricing` и `reference_pools` (в конфиге и коде) — там строки `dex` должны совпадать с нормализованным именем после миграции.
- Убедиться, что UI/оконный интерфейс (consumer Redis → фронт) понимает потенциальные новые `Exchange` значения, если они зависят от `dex`.
- Обновить документацию: `docs/project_documentation.md`, `docs/uniswap_v4_integration_plan.md`, README.

### Этап 9. Тестирование и проверка
- Юнит-тесты:
  - Новый пакет `basepools`: парсинг, фильтры, нормализация `dex`.
  - `util.LoadSymbolsFromFile` с фикстурами `base_pools`.
  - Регрессия для `NormalizePair`, `FinalizePool`.
- Интеграционные тесты:
  - Смоки `go test ./internal/dex/...` с моками RPC (минимальный набор пулов).
  - Локальный прогон `cmd/screener-core` с включенными DEX, проверка логов и содержимого Redis (например, `HGETALL price:uniswap_v3:WETHUSDC`).
- Нагрузочный прогон (минимум 30 минут) с реальными эндпоинтами Alchemy, оценка количества подписок и ошибок.

### Этап 10. Релиз и откат
- Добавить в релизные заметки: новый env `BASE_POOLS_JSON`, требования к обновлению файла.
- Подготовить fallback: возможность временно указать старый файл через переменную окружения, пока код не удалён (feature flag в загрузчике).
- После релиза наблюдать метрики Redis и логов коннекторов (ошибки парсинга, количество активных пулов). При проблемах — переключиться на резервный файл через конфиг.

## 5. Открытые вопросы
- Нужно ли фильтровать по минимальной ликвидности (`liquidity_usd`) на уровне загрузчика или оставить на усмотрение коннектора?
- Нужна ли поддержка не-Uniswap DEX (например, `pancakeswap`, `blackhole`) в текущих коннекторах — есть ли планы расширения?
- Требуется ли миграция существующих кэшей/словарей Redis, если названия пулов/ключей поменяются?

- Fallback: при отсутствии BASE_POOLS_JSON и shared_pools.file используем 	icker_source/base_pools.json. Апплаем по умолчанию.- Loader: util.LoadSymbolsFromFile теперь читает base_pools, поддерживает env SYMBOL_LOADER_AMM_FILTER и SYMBOL_LOADER_INCLUDE_STABLE.
- При фильтрации символов используем entry.symbol, сохранив stableSkipSet для legacy JSON.
- В cmd/screener-core обновлены вызовы LoadSymbolsFromFile: используются base_pools и оффлайн-фильтры без паники при пустых данных.
- Добавлены модульные тесты внутреннего загрузчика символов (фильтрация дубликатов, пустых символов и stable).
- Uniswap V2 коннектор грузит пулы через LoadPoolsFromBase* с фильтрацией по NormalizeDex(entry.dex) и mm_version == v2 (учитывает кастомные dex_cfg.name).
- V2 коннектор формирует PoolConfig из base_pools (ddress, токены, decimals, ориентация base/quote через ase_token/quote_token).
- V2 коннектор: BaseIsToken0 определяется по ase_token/quote_token, CanonicalPair строится как base+quote (uppercase), StableSymbol нормализуется через registry.
- V3 коннектор читает base_pools (фильтр NormalizeDex == uniswap, mm_version == v3, 
etwork == cfg.Network), метаданные строятся без Gecko.
