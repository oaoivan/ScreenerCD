package exchange

import (
    "encoding/json"
    "log"
    "time"
    "github.com/gorilla/websocket"
    "github.com/yourusername/screner/pkg/protobuf"
)

type UniswapClient struct {
    conn *websocket.Conn
}

func NewUniswapClient(url string) (*UniswapClient, error) {
    conn, _, err := websocket.DefaultDialer.Dial(url, nil)
    if err != nil {
        return nil, err
    }
    return &UniswapClient{conn: conn}, nil
}

func (uc *UniswapClient) Subscribe(symbol string) error {
    msg := map[string]interface{}{
        "type": "subscribe",
        "channel": "ticker",
        "symbol": symbol,
    }
    return uc.conn.WriteJSON(msg)
}

func (uc *UniswapClient) ReadMessages() (protobuf.MarketData, error) {
    _, message, err := uc.conn.ReadMessage()
    if err != nil {
        return protobuf.MarketData{}, err
    }

    var data protobuf.MarketData
    if err := json.Unmarshal(message, &data); err != nil {
        log.Printf("Error unmarshalling message: %v", err)
        return protobuf.MarketData{}, err
    }
    return data, nil
}

func (uc *UniswapClient) Close() {
    uc.conn.Close()
}

func (uc *UniswapClient) KeepAlive() {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            if err := uc.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
                log.Println("Error sending ping:", err)
                return
            }
        }
    }
}