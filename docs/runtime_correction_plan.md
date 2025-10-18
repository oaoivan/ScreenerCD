# План корректировки во время выполнения (2025-10-17)

## 1. Исправление конвейера Redis HSET
- Воспроизвести ошибку `ERR wrong number of arguments for 'hset'`, вызываемую в `internal/redisclient/client.go`.
- Проверить `HSetBatch`, чтобы убедиться, что мы вызываем `HSet(key, field, value)` или используем `HMSet` с правильной структурой аргументов (`key`, `field`, `value`, ...).
- Добавить модульный тест для вспомогательной функции пакетной обработки, чтобы отлавливать неправильно сформированные списки аргументов.
- Повторно запустить `go test ./internal/redisclient/...` и короткую сессию скринера, чтобы подтвердить успешную запись данных конвейером.

## 2. Корректировка данных базовых пулов
- Обновить генератор для `ticker_source/base_pools.json`, чтобы каждая запись Uniswap V3 имела действительный `pool_id`. Если внешний источник его не предоставляет, использовать `pool_address` или получать из метаданных on-chain.
- Пересобрать набор данных и убедиться, что Uniswap V3 может загрузить хотя бы ненулевой набор пулов (проверить логи после запуска: `uniswap_v3 loaded pools>0`).

## 3. Полнота метаданных Uniswap V4
- Исследовать записи, пропущенные с ошибкой `token0: empty address` и отсутствующими хуками. Убедиться, что набор данных записывает `token0.address`/`token1.address` и `pool_key.hooks`, когда это применимо.
- Повторно сгенерировать `base_pools.json`; подтвердить, что начальная загрузка V4 сохраняет ожидаемое количество пулов (лог `bootstrap pools done total=...`).

## 4. Регрессионное тестирование
- После применения исправлений запустить `go test ./...` и локально запустить `screener-core`.
- Проанализировать логи запуска (фильтры, количество пулов) и убедиться, что коннекторы CEX+DEX выдают цены без ошибок Redis.
- Задокументировать результат в `docs/base_pools_migration_plan.md` (в рамках выполнения этапа 9).
### RESP protocol compatibility investigation
- Verify runtime: capture HELLO handshake from current Redis (CLI + go-redis) to see negotiated RESP version.
- Reproduce error with minimal go-redis snippet after forcing RESP2 (Options.Protocol = 2) or sending HELLO 2; note server response.
- Evaluate upgrading to go-redis/v9 or alternative client if RESP2 forcing fails; document required code changes before implementation.
- go-redis/v8, go-redis/v9 и redigo на localhost:6379 воспроизводят `ERR wrong number of arguments for 'hset'` (Redis 8.2.1 stack). Для сравнения go-redis/v8 против локального `redis:7-alpine` (порт 6380) выполняет `HSET` успешно — подтверждает несовместимость конкретно с Redis 8.x.
- Поднял временный контейнер `redis:8.2.2-alpine` на 6381 и прогнал `HSET` через go-redis/v8 — всё записывается без ошибок.
- На штатном `redis:alpine` (8.2.1 + модули) ошибка сохраняется, значит регрессия исправлена в 8.2.2.
- Предлагаю обновить docker-compose до `redis:8.2.2-alpine` и перепроверить core.
