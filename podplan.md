# План рефакторинга Uniswap V3 коннектора

1. Анализ текущей реализации
   - Зафиксировать список глобальных переменных, используемых V3 коннектором.
   - Проверить, какие функции напрямую читают или модифицируют глобальное состояние.
   - Подготовить диаграмму потоков запуска (инициализация → загрузка пулов → подписки → обработка событий).

2. Проектирование новой структуры
   - Спроектировать структуру экземпляра (например, `v3Engine`) с полями вместо глобалов.
   - Определить интерфейсы/методы для диалера, загрузки пулов, пинга, чтения сообщений.
   - Согласовать формат конфигурации, чтобы один код обслуживал несколько сетей.

3. Миграция и рефакторинг кода
   - Перенести глобальные переменные в поля структуры и обновить функции на методы.
   - Обновить инициализацию `V3Connector` так, чтобы каждый инстанс создавал свой `v3Engine`.
   - Переписать подписку и обработку событий с учётом изолированного состояния.

4. Тестирование и проверка
   - Написать/обновить юнит-тесты для загрузки пулов и нормализации фильтров.
   - Запустить smoke (`go run ./cmd/screener-core/main.go`) и убедиться, что оба инстанса V3 появляются в логах.
   - Проверить Redis на наличие ключей `price:*:uniswap_v3:*` для Ethereum и BSC.

5. Чистка и документация
   - Удалить мёртвый код и комментарии, актуализировать логирование.
   - Обновить внутренние документы/README о поддержке нескольких инстансов V3.
   - Подготовить список follow-up задач (оптимизации, мониторинг, дополнительные тесты).

## Шаг 1 — Анализ текущей реализации

### Глобальное состояние
| Переменная | Роль | Запись | Чтение |
| --- | --- | --- | --- |
| `v3Pools map[common.Address]*v3PoolMeta` | каталог пулов | `v3LoadPools` (создаёт), поля мутируют `v3EnsurePoolMeta`, `v3MaybeSetWETHUSD` | `v3SubscribeAll`, `v3Process`, `v3HandleSwap`, `v3DeriveUSD` |
| `v3BatchSize int` | размер батча подписки | `v3Run` | `v3SubscribeAll` |
| `v3ABI abi.ABI`, `v3EventBySig map[common.Hash]abi.Event` | кэш ABI | `v3InitABI` | `v3Process`, `v3HandleSwap` |
| `v3TwoPow192 *big.Int` | константа 2^192 | инициализация пакета | `v3PriceFromSqrt` |
| `v3WSURL string`, `v3HTTPURL string` | конечные точки RPC | `v3ResolveWS` | `v3Dial`, `v3EthCall`, логи |
| `v3PingInterval time.Duration` | пинг интервал | `v3Run` | `v3Ping` |
| `v3StopOnErrAck`, `v3LogAllEvents`, `v3DecodeSwapOnly` | флаги поведения | `v3Run` | `v3Handle`, `v3Process`, `v3HandleSwap`, `v3EmitUSDWithPricer` |
| `v3HTTPClient *http.Client` | общий HTTP клиент | объявление | `v3EthCall` |
| `v3TokenCache map[common.Address]v3TokenMeta`, `v3TokenMu` | кэш метаданных токенов | `v3Run` (reset), `v3FetchTokenMeta` (update) | `v3FetchTokenMeta` |
| `v3Pow10Cache map[uint8]*big.Int`, `v3Pow10Mu` | кэш степеней 10 | `v3Run` (reset) | `v3Pow10`, `v3AmountToFloat` |
| `v3Registry *TokenRegistry` | глобальная ссылка на реестр | `v3InitAssets` | `v3IsStableSymbol`, `v3RegisterToken` |
| `v3StableLookup map[string]struct{}` | набор стейблов | `init`, `v3InitAssets` | `v3IsStableSymbol`, `v3MaybeSetWETHUSD` |
| `v3WETHAddress common.Address` | адрес WETH | объявление, `v3InitAssets` | `v3DeriveUSD`, `v3MakeTokenInfo` |
| `v3WETHUSD *big.Rat`, `v3WETHUSDStable string`, `v3WethMu` | последняя цена WETHUSD | `v3Run` (reset), `v3MaybeSetWETHUSD` | `v3DeriveUSD`, `v3EmitUSDWithPricer` |
| `v3MetaSem chan struct{}` | семафор загрузки метаданных | `v3Run` | `v3TryAcquireMetaSlot`, `v3ReleaseMetaSlot` |
| `v3OutChan chan<- *pb.MarketData`, `v3OutMu` | общий выходной канал | `v3Run` (assign/clear) | `v3Publish`, `v3EmitUSDWithPricer` |
| `v3ExchangeName string`, `v3NetworkName string`, `v3ChainID uint64` | идентификаторы | `v3Run` | `v3Publish`, `v3MakeTokenInfo`, `v3EmitUSDWithPricer` |
| `v3Pricer pricing.Pricer`, `v3PricerMu` | ссылка на прайсер | `v3Run` | `v3RegisterToken`, `v3UpdatePricing`, `v3EmitUSDWithPricer`, `v3CurrentPricer` |
| `v3MessageCount uint64`, `v3ReconnectCount uint64` | счётчики | `v3Run` (reset), `atomic.Add*` внутри `v3Publish`, `v3RunLoop` | отчёт в логах |
| `v3FallbackStableSymbols []string` | дефолтные стейблы | объявление | `v3FallbackStableSet` |

### Потоки вызовов
1. `V3Connector.Run` → `v3Run` подготавливает ABI, подключение, пула, регистр, глобальные кэши и каналы.
2. `v3Run` → `v3RunLoop` запускает цикл reconnect с `v3Dial`, `v3SubscribeAll`, горутинами `v3Ping` и `v3Read`, счётчиком `v3ReconnectCount`.
3. `v3Read` → `v3Handle` → `v3Process` выбирает события, обращается к `v3Pools`, фильтрам и вызывает `v3HandleSwap`.
4. `v3HandleSwap` инициирует `v3EnsurePoolMeta`, `v3DeriveUSD`, `v3EmitPair`, `v3UpdatePricing`, которые используют `v3OutChan`, `v3Pricer`, `v3TokenCache`, `v3WETHUSD`.

### Выводы
- Все ключевые зависимости (`ws/http urls`, `пулы`, `прайсер`, `выходной канал`) живут в глобальном пространстве, что блокирует независимые инстансы.
- Вспомогательные структуры (`token cache`, `WETHUSD`, `meta sem`) также singleton и не защищены от одновременного использования разными сетями.
- Сброс состояний в `v3Run` перезатирает данные других запусков, если коннекторов несколько.

## Шаг 2 — Проектирование новой структуры

- Свериться с `internal/dex/Etherium/Uniswap` архитектурой и коннекторами V2/V4.
- Нарисовать структуру `v3Engine` (pools, cfg, pricer, io, мета-кэши, счетчики).
- Определить интерфейсы: `dialer`, `poolLoader`, `wsReader`, `metaFetcher`.
- Решить формат конфигурации: сеть, rpc, фильтры, батчи, workers.
- Описать жизненный цикл engine: `Init -> Connect -> Subscribe -> Loop -> Shutdown`.
- Зафиксировать в документе схему потоков и ответственности модулей.

### Структура экземпляра

```go
type v3Engine struct {
   ctx       context.Context
   cancel    context.CancelFunc

   cfg       V3Config        // неизменяемая конфигурация инстанса
   logPrefix string          // `[uniswap_v3:eth]`

   pricer    pricing.Pricer  // внешний прайсер, используется через методы

   // Пулы и метаданные
   pools     map[common.Address]*v3PoolMeta
   poolsMu   sync.RWMutex

   tokenCache map[common.Address]v3TokenMeta
   tokenMu    sync.RWMutex
   pow10Cache map[uint8]*big.Int
   pow10Mu    sync.RWMutex

   // RPC и подписки
   wsURL      string
   httpURL    string
   httpClient *http.Client
   dialer     v3Dialer
   subscriber v3Subscriber
   reader     v3WSReader

   pingInterval time.Duration
   batchSize    int
   stopOnAckErr bool
   decodeSwapOnly bool
   logAllEvents bool

   // Выходной канал и метрики
   out         chan<- *pb.MarketData
   exchange    string
   network     string
   chainID     uint64

   messageCount   uint64
   reconnectCount uint64

   // WETH и стейблы
   registry     *TokenRegistry
   stableLookup map[string]struct{}
   wethAddress  common.Address
   wethUSD      *big.Rat
   wethStable   string
   wethMu       sync.RWMutex

   // Семантика метаданных
   metaSem chan struct{}

   // Служебные горутины
   wg sync.WaitGroup
}
```

### Интерфейсы зависимостей

- `type v3Dialer interface { Dial(ctx context.Context, url string) (*websocket.Conn, error) }` — отдельный слой, чтобы подменять транспорт в тестах.
- `type v3Subscriber interface { Subscribe(ctx context.Context, conn *websocket.Conn, pools []common.Address, batch int) error }` — управляет батчами и ack логикой.
- `type v3WSReader interface { Run(ctx context.Context, conn *websocket.Conn, out chan<- []byte) error }` — читает сообщения, обрабатывает ping/pong.
- `type v3MetaFetcher interface { EnsurePoolMeta(ctx context.Context, pool *v3PoolMeta) error }` — капсулирует `eth_call` и кэш токенов.

В бою все интерфейсы реализует сама структура, но тесты смогут подложить моки.

### Методы экземпляра

- `func newV3Engine(ctx context.Context, cfg V3Config, pr pricing.Pricer, out chan<- *pb.MarketData) (*v3Engine, error)` — подготавливает поля, резолвит URLs, инициализирует кэши, семафор и лог-префикс.
- `func (e *v3Engine) Run() error` — точка входа; загружает пулы, стартует цикл подключений, ждёт `ctx.Done`.
- `func (e *v3Engine) initABI() error` — один раз загружает ABI в поле `abi parsedABI` внутри структуры (добавить поле `abi abi.ABI`).
- `func (e *v3Engine) loadPools() error` — читает JSON/источник, фильтрует, заполняет `e.pools`.
- `func (e *v3Engine) connectLoop() error` — эквивалент `v3RunLoop`; следит за reconnect/backoff, обновляет счётчики.
- `func (e *v3Engine) handleMessage(raw []byte)` — заменяет `v3Handle`, разбирает ack/notification.
- `func (e *v3Engine) processLog(item v3LogItem)` — фильтрует события и ищет пул.
- `func (e *v3Engine) handleSwap(pool *v3PoolMeta, item v3LogItem)` — бывший `v3HandleSwap`.
- `func (e *v3Engine) ensurePoolMeta(pool *v3PoolMeta) error` — использует `e.tokenCache`, `e.metaSem`.
- `func (e *v3Engine) registerToken(meta v3TokenMeta)` — регистрирует токен в `pricing.Pricer` и стейблах.
- `func (e *v3Engine) emitPair(base, quote string, price *big.Rat)` — отправляет маркет-дату; учитывает `e.exchange`, `e.network`, `e.chainID`.
- `func (e *v3Engine) deriveUSD(pool *v3PoolMeta, price1Per0, price0Per1 *big.Rat) string` — оперирует `e.wethUSD`, `e.stableLookup`.
- `func (e *v3Engine) shutdown()` — закрывает соединения, ждёт `wg`, обнуляет ссылки.

Каждый публичный метод будет логировать через `util.Infof`/`Debugf` с добавлением `e.logPrefix` (например, `util.Infof("%s reconnect #%d", e.logPrefix, count)`).

### Жизненный цикл

1. `newV3Engine` → resolve config, создать кэши, семафор, registry.
2. `Run` → `initABI`, `loadPools`, `initAssets`, `connectLoop`.
3. `connectLoop` → `dialer.Dial`, `subscriber.Subscribe`, запустить горутины `ping`, `reader`, `handler`.
4. `handleMessage` → `processLog` → `handleSwap` → `emitPair`/`deriveUSD`/`updatePricing`.
5. По `ctx.Done` → `shutdown`.

Такой скелет повторяет подход коннекторов V2/V4 и позволяет поднять несколько сетей параллельно без конфликтов по глобальному состоянию.

### Конфигурация инстанса

| Поле `V3Config` | Назначение | Пример | Обработка в `v3Engine` |
| --- | --- | --- | --- |
| `Exchange` | Алиас для логов/выхода | `uniswap_v3` | нормализуем в `exchange` |
| `Network` | Человекочитаемое имя сети | `ethereum` / `bsc` | lower-case в `network` |
| `ChainID` | Целевой ChainID | `1`, `56` | fallback к `registry.ChainID()` |
| `WSURL` | Прямой WS endpoint | `wss://...` | сохраняем в `wsURL` |
| `HTTPURL` | RPC для `eth_call` | `https://...` | сохраняем в `httpURL` |
| `PoolsPath` | Путь к JSON | `ticker_source/uniswap_v4_pools.json` | `loadPools()` |
| `DexFilter(s)` | Ограничение по DEX | `Uniswap` | фильтрация при загрузке |
| `NetworkFilter(s)` | Ограничение по сети | `ethereum` | фильтрация при загрузке |
| `AMMVersions` | Ограничение по версии AMM | `[]string{"v3"}` | фильтрация при загрузке |
| `BatchSize` | Размер батча подписок | `150` | `batchSize` с дефолтом |
| `PingInterval` | Интервал ping | `25s` | `pingInterval` с дефолтом |
| `StopOnAckError` | Остановка при ack error | `false` | `stopOnAckErr` |
| `LogAllEvents` | Глобальное логирование | `false` | `logAllEvents` |
| `DecodeSwapOnly` | Фильтровать non-swap | `true` | `decodeSwapOnly` |
| `MaxMetaWorkers` | Параллелизм метаданных | `8` | размер `metaSem` |
| `Registry` | Внешний registry | `*TokenRegistry` | `initAssets` |
| `WantedPairs` | Ограничение по парам | `[]string{"ETHUSDT"}` | применить после загрузки |

Дополнительно: `newV3Engine` должен уметь принимать overrides из ENV (как сейчас делает `v3ResolveWS`). Продумать расширение конфигурации для нестандартных RPC (например, отдельный HTTP URL для метаданных).

### Ответственности модулей

- `v3Engine` — orchestration; хранит состояние, координирует lifecycle, отвечает за логи.
- `dialer` — сетевое подключение с возможностью переопределения (тесты, прокси).
- `subscriber` — батчит адреса и управляет ack-ошибками, возвращает идентификаторы подписок (для InMemory-тестов можно эмулировать ack).
- `reader` — единая точка чтения WS; обрабатывает ping/pong, закрытие, трассирует ошибки.
- `metaFetcher` — `eth_call`, кэш токенов, WETHUSD, регистрация стейблов.
- `pricingAdapter` (методы `registerToken`, `updatePricing`) — оболочка поверх `pricing.Pricer`, чтобы облегчить мок.

Компоненты общаются через явные методы структуры. Глобальные функции (`v3DecodeInt256`, `v3Format`, `v3Mask` и т.п.) можно оставить пакетными утилитами, если они stateless.

## Шаг 3 — Миграция и рефакторинг кода

- Создать `v3Engine` со всеми полями, конструктор и `Run` метод.
- Перенести в структуру все глобали (`pools`, `cfg`, `tokenCache`, `wethUSD`, `sem` и т.д.).
- Превратить функции `v3Run`, `v3RunLoop`, `v3Handle`, `v3HandleSwap` в методы структуры.
- Обновить вызовы в `V3Connector` и регистрах, убрать зависимость от пакета-глобалов.
- Обновить вспомогательные функции (`v3EnsurePoolMeta`, `v3DeriveUSD`, `v3Publish`) на методы.
- Добавить контекст отмены и логи, чтобы каждый инстанс логировал свой префикс.

## Шаг 4 — Тестирование и проверка

- Написать тесты на загрузку пулов с фильтрами (мок JSON) в `internal/dex/.../test_scripts`.
- Добавить тест на кэш степеней и токенов (проверить изоляцию между инстансами).
- Прогнать smoke `go run ./cmd/screener-core/main.go` для двоих сетей, собрать логи.
- Проверить Redis на ключи `price:*:uniswap_v3:*` и убедиться, что ETH/BSC идут параллельно.
- Измерить лаг подписки и убедиться, что reconnect не сбрасывает соседний инстанс.

## Шаг 5 — Чистка и документация

- Проинвентаризировать старые глобальные функции, удалить дубли и мертвый код.
- Обновить README/docs по запуску нескольких сетей и описанию конфига.
- Настроить доп. логирование (сеть, chainID) и алерты на reconnect.
- Составить backlog для будущих оптимизаций (batch tuning, пулы из Redis, мониторинг).
