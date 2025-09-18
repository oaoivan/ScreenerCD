package exchange

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yourusername/screner/internal/util"
	"github.com/yourusername/screner/pkg/protobuf"
)

type BybitClient struct {
	wsConn *websocket.Conn
	url    string
	out    chan *protobuf.MarketData
}

func NewBybitClient(url string) *BybitClient {
	util.Debugf("creating Bybit client for url=%s", url)
	return &BybitClient{url: url, out: make(chan *protobuf.MarketData)}
}

func (c *BybitClient) Out() <-chan *protobuf.MarketData {
	return c.out
}

func (c *BybitClient) Connect() error {
	var err error
	util.Infof("connecting to Bybit WS %s", c.url)
	c.wsConn, _, err = websocket.DefaultDialer.Dial(c.url, nil)
	if err != nil {
		util.Errorf("Bybit connect error: %v", err)
		return err
	}
	util.Infof("Bybit connected")
	return nil
}

func (c *BybitClient) Subscribe(symbol string) error {
	util.Infof("Bybit subscribing to %s", symbol)
	// use tickers channel for price updates in v5 public spot
	msg := map[string]interface{}{
		"op":   "subscribe",
		"args": []string{"tickers." + symbol},
	}
	if err := c.wsConn.WriteJSON(msg); err != nil {
		util.Errorf("Bybit subscribe error: %v", err)
		return err
	}
	return nil
}

func (c *BybitClient) ReadLoop(exchangeName, symbol string) {
	defer func() {
		util.Infof("Bybit ReadLoop for %s exiting", exchangeName)
		c.wsConn.Close()
	}()
	for {
		_, message, err := c.wsConn.ReadMessage()
		if err != nil {
			util.Errorf("Bybit read error: %v", err)
			close(c.out)
			return
		}

		util.Debugf("Bybit raw message: %s", string(message))

		var raw map[string]interface{}
		if err := json.Unmarshal(message, &raw); err != nil {
			util.Errorf("Error unmarshalling message: %v", err)
			continue
		}

		var price float64
		var messageSymbol string

		// Extract symbol from topic (e.g., "tickers.BTCUSDT" -> "BTCUSDT")
		if topic, ok := raw["topic"].(string); ok {
			if len(topic) > 8 && topic[:8] == "tickers." {
				messageSymbol = topic[8:] // Extract symbol after "tickers."
			}
		}

		// Bybit v5 tickers format: {"topic":"tickers.SYMBOL","data":{"lastPrice":"123.45",...}}
		if data, ok := raw["data"].(map[string]interface{}); ok {
			if lastPriceStr, ok := data["lastPrice"].(string); ok {
				if p, err := strconv.ParseFloat(lastPriceStr, 64); err == nil {
					price = p
				} else {
					util.Errorf("Bybit failed to parse lastPrice '%s': %v", lastPriceStr, err)
					continue
				}
			}
		}

		if price == 0 || messageSymbol == "" {
			util.Debugf("Bybit message skipped (no price or symbol)")
			continue
		}

		util.Debugf("Bybit parsed price=%f for %s/%s", price, exchangeName, messageSymbol)

		md := &protobuf.MarketData{
			Exchange:  exchangeName,
			Symbol:    messageSymbol, // Use symbol from message topic
			Price:     price,
			Timestamp: time.Now().Unix(),
		}

		select {
		case c.out <- md:
		default:
			// If nobody consumes, drop to avoid blocking
			util.Debugf("Bybit drop message, no consumer")
		}
	}
}

func (c *BybitClient) Close() {
	if c.wsConn != nil {
		util.Infof("closing Bybit client")
		c.wsConn.Close()
	}
}

func (c *BybitClient) KeepAlive() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := c.wsConn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				util.Errorf("Error sending ping: %v", err)
				return
			}
			util.Debugf("Bybit ping sent")
		}
	}
}
