package exchange

import (
    "encoding/json"
    "log"
    "time"
    "github.com/gorilla/websocket"
    "github.com/yourusername/screner/pkg/protobuf"
)

type GateioConnector struct {
    ws *websocket.Conn
    url string
}

func NewGateioConnector(url string) *GateioConnector {
    return &GateioConnector{
        url: url,
    }
}

func (g *GateioConnector) Connect() error {
    var err error
    g.ws, _, err = websocket.DefaultDialer.Dial(g.url, nil)
    if err != nil {
        return err
    }
    return nil
}

func (g *GateioConnector) Subscribe(symbol string) error {
    msg := map[string]interface{}{
        "id":     1,
        "method": "ticker.subscribe",
        "params": []string{symbol},
    }
    return g.ws.WriteJSON(msg)
}

func (g *GateioConnector) ReadMessages() (<-chan protobuf.MarketData, error) {
    marketDataChan := make(chan protobuf.MarketData)

    go func() {
        defer close(marketDataChan)
        for {
            _, message, err := g.ws.ReadMessage()
            if err != nil {
                log.Println("Error reading message:", err)
                return
            }

            var data map[string]interface{}
            if err := json.Unmarshal(message, &data); err != nil {
                log.Println("Error unmarshalling message:", err)
                continue
            }

            if ticker, ok := data["ticker"].(map[string]interface{}); ok {
                marketData := protobuf.MarketData{
                    Exchange: "Gateio",
                    Symbol:   ticker["symbol"].(string),
                    Price:    ticker["last"].(float64),
                    Timestamp: time.Now().Unix(),
                }
                marketDataChan <- marketData
            }
        }
    }()

    return marketDataChan, nil
}

func (g *GateioConnector) Close() error {
    return g.ws.Close()
}