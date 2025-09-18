<img src="https://r2cdn.perplexity.ai/pplx-full-logo-primary-dark%402x.png" style="height:64px;margin-right:32px"/>

# найди на сайте биржы bybit в документации по апи как конкретно подключить через вэбсокет заданное количество интсрументов

На официальном Node.js SDK Bybit для работы с WebSocket, чтобы подключиться и подписаться на заданное количество инструментов, используется класс WebsocketClient. Для подписки на несколько тем (инструментов) одновременно применяют метод subscribeV5, где в виде массива передают список интересующих топиков (инструментов).

Пример (JavaScript):

```javascript
const { WebsocketClient } = require('bybit-api');

const wsConfig = {
  key: 'YOUR_API_KEY',
  secret: 'YOUR_API_SECRET',
  market: 'v5'  // использование v5 рынка
};

const ws = new WebsocketClient(wsConfig);

// Подписка на несколько инструментов (топиков) сразу, например, стаканы по BTCUSDT и ETHUSDT на линейных контрактах
ws.subscribeV5(['orderbook.50.BTCUSDT', 'orderbook.50.ETHUSDT'], 'linear');

// Обработка входящих данных
ws.on('update', (data) => {
  console.log('update', data);
});
```

Таким образом, для подключения и подписки на заданное количество инструментов через WebSocket по API Bybit нужно создать WebsocketClient с market: 'v5' и вызвать subscribeV5, передав массив инструментов и категорию рынка (например, 'linear').[^1_1][^1_2]
<span style="display:none">[^1_10][^1_3][^1_4][^1_5][^1_6][^1_7][^1_8][^1_9]</span>

<div style="text-align: center">⁂</div>

[^1_1]: https://app.unpkg.com/bybit-api@3.10.27/files/README.md

[^1_2]: https://www.npmjs.com/package/bybit-api/v/3.7.4

[^1_3]: https://stackoverflow.com/questions/73984183/cannot-subscribe-websocket-for-inverse-contract-in-bybit-unified-v3-api

[^1_4]: https://vezgo.com/blog/bybit-api-cheat-sheet-for-developers/

[^1_5]: https://bybit-exchange.github.io/docs/v5/ws/connect

[^1_6]: https://www.bybit.com/future-activity/en/developer

[^1_7]: https://github.com/hirokisan/bybit/issues/181

[^1_8]: https://bybit-exchange.github.io/docs/changelog/v5

[^1_9]: https://dev.to/kylefoo/bybits-pybit-how-to-subscribe-to-kline-websocket-stream-5c2f

[^1_10]: https://learn.bybitglobal.com/en/bybit-guide/how-to-create-a-bybit-api-key


---

# мне нужно подписаться на спот котировки, приведи пример

Для подписки на спотовые котировки через WebSocket на Bybit API нужно подключиться к публичному спотовому WebSocket-эндпоинту:

- Mainnet: `wss://stream.bybit.com/v5/public/spot`
- Testnet: `wss://stream-testnet.bybit.com/v5/public/spot`

После подключения отправляется запрос на подписку с операцией `"subscribe"`, в котором в массиве `"args"` указываются нужные инструменты и топики.

Пример запроса подписки на спотовый стакан (orderbook) по нескольким инструментам (например, BTCUSDT и ETHUSDT):

```json
{
  "op": "subscribe",
  "args": [
    "orderbook.1.BTCUSDT",
    "orderbook.1.ETHUSDT"
  ]
}
```

Для поддержания соединения рекомендуется отправлять ping-запросы каждые 20 секунд.

Таким образом, для подписки на спотовые котировки нужно:

1. Подключиться к `wss://stream.bybit.com/v5/public/spot`
2. Отправить сообщение подписки с нужными инструментами в `"args"`
3. Обрабатывать получаемые обновления с котировками

Это пример простой подписки на 1-уровневый стакан для двух спотовых инструментов через WebSocket Bybit API.[^2_1]

<div style="text-align: center">⁂</div>

[^2_1]: https://bybit-exchange.github.io/docs/v5/ws/connect


---

# еще раз мне нужен не стакан а просто котировки. это первое. второе зачем отправлять каждые 20 секунд запросы этоже вэбсокет. он и нужен как раз для того чтобы не отправлять запросы

По спотовым котировкам (тикерам) на Bybit через WebSocket подписка делается так:

1. Подключаетесь к WebSocket публичному эндпоинту спота:
`wss://stream.bybit.com/v5/public/spot`
2. Отправляете сообщение подписки с операцией `"subscribe"` и аргументом `"tickers.{instrument}"` — например, для BTCUSDT и ETHUSDT:
```json
{
  "op": "subscribe",
  "args": [
    "tickers.BTCUSDT",
    "tickers.ETHUSDT"
  ]
}
```

3. Получаете в ответ поток реального времени с данными по последней цене, объему и другим котировочным параметрам.

Важно: WebSocket сам поддерживает постоянное соединение и автоматически присылает обновления, поэтому периодические пинги (ping-запросы) нужны только для поддержания соединения активным (по протоколу WebSocket), а не для запроса данных. Данные не нужно заново запрашивать — обновления поступают по подписке.

Таким образом, для подписки на спот котировки просто отправьте `"tickers.{symbol}"` в `"args"` при подключении.[^3_1][^3_2][^3_3]
<span style="display:none">[^3_10][^3_4][^3_5][^3_6][^3_7][^3_8][^3_9]</span>

<div style="text-align: center">⁂</div>

[^3_1]: https://gealber.com/bybit-websocket-tickers

[^3_2]: https://bybit-exchange.github.io/docs/v5/ws/connect

[^3_3]: https://bybit-exchange.github.io/docs/v5/websocket/public/ticker

[^3_4]: https://www.esegece.com/help/sgcWebSockets/Components/APIs/API/API_Bybit.htm

[^3_5]: https://wundertrading.com/journal/en/learn/article/bybit-api

[^3_6]: https://www.youtube.com/watch?v=dQxCPkYtPhw

[^3_7]: https://www.bybit.com/future-activity/en/developer

[^3_8]: https://github.com/JKorf/Bybit.Net

[^3_9]: https://stackoverflow.com/questions/72563391/how-to-build-a-simple-websocket-in-python-for-bybit-and-fetch-the-values-in-vari

[^3_10]: https://announcements.bybit.com/article/introducing-new-api-feature-websocket-order-placement-blt9d3dc36eff27f1c1/

