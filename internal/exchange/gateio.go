package exchange

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yourusername/screner/internal/util"
	"github.com/yourusername/screner/pkg/protobuf"
)

// GateClient implements minimal WS client for Gate.io v4 public spot tickers
type GateClient struct {
	wsConn *websocket.Conn
	url    string
	out    chan *protobuf.MarketData
}

func NewGateClient(url string) *GateClient {
	util.Debugf("creating Gate client for url=%s", url)
	return &GateClient{url: url, out: make(chan *protobuf.MarketData)}
}

func (c *GateClient) Out() <-chan *protobuf.MarketData { return c.out }

func (c *GateClient) Connect() error {
	var err error
	util.Infof("connecting to Gate WS %s", c.url)
	c.wsConn, _, err = websocket.DefaultDialer.Dial(c.url, nil)
	if err != nil {
		util.Errorf("Gate connect error: %v", err)
		return err
	}
	util.Infof("Gate connected")
	return nil
}

// Subscribe subscribes to spot.tickers for one symbol in Gate format (e.g., BTC_USDT)
func (c *GateClient) Subscribe(gateSymbol string) error {
	util.Infof("Gate subscribing to %s", gateSymbol)
	msg := map[string]interface{}{
		"time":    time.Now().Unix(),
		"channel": "spot.tickers",
		"event":   "subscribe",
		"payload": []string{gateSymbol},
	}
	if err := c.wsConn.WriteJSON(msg); err != nil {
		util.Errorf("Gate subscribe error: %v", err)
		return err
	}
	return nil
}

func (c *GateClient) ReadLoop(exchangeName string) {
	defer func() {
		util.Infof("Gate ReadLoop exiting")
		c.wsConn.Close()
	}()
	for {
		_, message, err := c.wsConn.ReadMessage()
		if err != nil {
			util.Errorf("Gate read error: %v", err)
			close(c.out)
			return
		}

		util.Debugf("Gate raw message: %s", string(message))

		var raw map[string]interface{}
		if err := json.Unmarshal(message, &raw); err != nil {
			util.Errorf("Gate unmarshal error: %v", err)
			continue
		}

		// Expect channel spot.tickers and event update
		if ch, _ := raw["channel"].(string); ch != "spot.tickers" {
			continue
		}
		// Some messages may carry "result" as object
		res, ok := raw["result"].(map[string]interface{})
		if !ok {
			continue
		}
		lastStr, _ := res["last"].(string)
		if lastStr == "" {
			// sometimes price may be numeric; try other forms
			if f, ok := res["last"].(float64); ok {
				lastStr = strconv.FormatFloat(f, 'f', -1, 64)
			}
		}
		if lastStr == "" {
			continue
		}
		price, err := strconv.ParseFloat(lastStr, 64)
		if err != nil {
			util.Errorf("Gate parse price error: %v", err)
			continue
		}
		sym, _ := res["currency_pair"].(string)
		if sym == "" {
			continue
		}

		md := &protobuf.MarketData{
			Exchange:  exchangeName,
			Symbol:    sym,
			Price:     price,
			Timestamp: time.Now().Unix(),
		}
		select {
		case c.out <- md:
		default:
			util.Debugf("Gate drop message, no consumer")
		}
	}
}

func (c *GateClient) Close() {
	if c.wsConn != nil {
		util.Infof("closing Gate client")
		c.wsConn.Close()
	}
}

func (c *GateClient) KeepAlive() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if err := c.wsConn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
			util.Errorf("Gate ping error: %v", err)
			return
		}
		util.Debugf("Gate ping sent")
	}
}
