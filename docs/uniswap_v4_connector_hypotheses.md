# Uniswap V4 connector: возможные причины отсутствия данных в Redis

## 1. PoolId из конфигурации может не совпасть с идентификатором в логах
- Коннектор формирует карту пулов на этапе bootstrap и ключом использует `common.Hash` из поля `PoolID`. 【F:internal/dex/Etherium/Uniswap/v4_connector.go†L430-L499】
- При обработке события `Swap` PoolId читается из `topics[1]` и ищется в этой карте; если совпадения нет, событие игнорируется. 【F:internal/dex/Etherium/Uniswap/v4_connector.go†L1044-L1138】
- В источнике `ticker_source/geckoterminal_pools.json` поле `pool_id` зачастую совпадает с адресом пула, однако в V4 идентификатором события выступает hash `PoolKey` (валюты, fee, hook). Если файл содержит адрес контракта, а не bytes32 идентификатор, коннектор не найдёт пул и не дойдёт до публикации котировки.

## 2. Подписка ограничена только адресом PoolManager
- Запрос `eth_subscribe` фильтрует логи исключительно по адресу `PoolManager`. 【F:internal/dex/Etherium/Uniswap/v4_connector.go†L762-L781】
- На сети Base/Arbitrum часть интеграторов транслирует swap-ивенты через hook или vault контракты, а PoolManager эмитит лишь административные события. Если провайдер отдаёт свапы не с того адреса, коннектор вообще не увидит торговых логов.

## 3. Нативные токены с нулевым адресом не регистрируются в прайсере
- Метод `registerToken` пропускает токены с `Address == 0x0`, поэтому ETH/BASE и любые wrapped-native без явного адреса не попадают в `GraphPricer`. 【F:internal/dex/Etherium/Uniswap/v4_connector.go†L200-L211】
- Сам прайсер также игнорирует пары, где хотя бы один адрес пустой: `UpdatePair` и `ResolveUSD` сразу возвращают управление. 【F:internal/dex/pricing/graph_pricer.go†L205-L230】【F:internal/dex/pricing/graph_pricer.go†L393-L398】
- В результате не строятся USD-маршруты, `ResolveUSD` всегда `false`, а `emitUSD` ничего не публикует. Если downstream пайплайн ждёт именно USD-цену от V4 (например, для ключей `price_canon`), Redis так и остаётся пустым.

## 4. Ошибки нормализации decimals приводят к отбрасыванию цены
- Цена строится через `sqrtPriceToDirectionalPrices`, где decimals берутся из конфига/JSON. 【F:internal/dex/Etherium/Uniswap/v4_connector.go†L1086-L1105】
- Если decimals указаны некорректно (например, Gecko отдаёт `null` или нестандартное значение, а Registry не знает токен), то `ratToFloat64` вернёт NaN/Inf и котировка будет отброшена, т.к. `updatePricing` и `emitSpot` требуют `price?Valid`. 【F:internal/dex/Etherium/Uniswap/v4_connector.go†L1101-L1185】
- Без запроса к `HTTPURL` для валидации метаданных (параметр обязательный, но нигде не используется) риск получить неверные decimals остаётся высоким, особенно для новых пулов, поэтому ценовые апдейты могут не доходить до Redis.

