package exchange

import (
    "encoding/json"
    "log"
    "time"
    "github.com/gorilla/websocket"
    "github.com/yourusername/screner/pkg/protobuf"
)

type OKXConnector struct {
    wsClient *websocket.Conn
    url      string
}

func NewOKXConnector(url string) *OKXConnector {
    return &OKXConnector{
        url: url,
    }
}

func (c *OKXConnector) Connect() error {
    var err error
    c.wsClient, _, err = websocket.DefaultDialer.Dial(c.url, nil)
    if err != nil {
        log.Printf("Failed to connect to OKX: %v", err)
        return err
    }
    log.Println("Connected to OKX")
    return nil
}

func (c *OKXConnector) Subscribe(symbol string) error {
    subscribeMessage := map[string]interface{}{
        "op":   "subscribe",
        "args": []string{symbol},
    }
    return c.wsClient.WriteJSON(subscribeMessage)
}

func (c *OKXConnector) ReadMessages() (protobuf.MarketData, error) {
    var marketData protobuf.MarketData
    _, message, err := c.wsClient.ReadMessage()
    if err != nil {
        return marketData, err
    }

    err = json.Unmarshal(message, &marketData)
    if err != nil {
        return marketData, err
    }

    marketData.Exchange = "OKX"
    marketData.Timestamp = time.Now().Unix()
    return marketData, nil
}

func (c *OKXConnector) Close() {
    if c.wsClient != nil {
        c.wsClient.Close()
    }
}