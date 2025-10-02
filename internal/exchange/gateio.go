package exchange

import (
	"encoding/json"
	"strconv"
	"strings"
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
	util.Debugf("Gate subscribing to %s", gateSymbol)
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

		// Strict typed decode with flexible price parsing
		type gateResult struct {
			CurrencyPair string          `json:"currency_pair"`
			Last         json.RawMessage `json:"last"`
		}
		type gateMsg struct {
			Channel string      `json:"channel"`
			Event   string      `json:"event"`
			Result  *gateResult `json:"result"`
		}
		var m gateMsg
		if err := json.Unmarshal(message, &m); err != nil {
			util.Errorf("Gate unmarshal error: %v", err)
			continue
		}
		if m.Channel != "spot.tickers" || m.Result == nil || m.Result.CurrencyPair == "" {
			continue
		}
		// Parse price from RawMessage that can be string or number
		var lastStr string
		if len(m.Result.Last) > 0 {
			// try string first
			var s string
			if err := json.Unmarshal(m.Result.Last, &s); err == nil {
				lastStr = s
			} else {
				var f float64
				if err2 := json.Unmarshal(m.Result.Last, &f); err2 == nil {
					lastStr = strconv.FormatFloat(f, 'f', -1, 64)
				}
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
		sym := m.Result.CurrencyPair

		md := &protobuf.MarketData{
			Exchange:  strings.ToLower(exchangeName),
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
