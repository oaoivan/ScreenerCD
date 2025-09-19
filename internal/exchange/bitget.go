package exchange

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yourusername/screner/internal/util"
	"github.com/yourusername/screner/pkg/protobuf"
)

// BitgetClient implements WS client for Bitget public v2 spot ticker
type BitgetClient struct {
	wsConn *websocket.Conn
	url    string
	out    chan *protobuf.MarketData
}

func NewBitgetClient(url string) *BitgetClient {
	util.Debugf("creating Bitget client for url=%s", url)
	return &BitgetClient{url: url, out: make(chan *protobuf.MarketData)}
}

func (c *BitgetClient) Out() <-chan *protobuf.MarketData { return c.out }

func (c *BitgetClient) Connect() error {
	var err error
	util.Infof("connecting to Bitget WS %s", c.url)
	c.wsConn, _, err = websocket.DefaultDialer.Dial(c.url, nil)
	if err != nil {
		util.Errorf("Bitget connect error: %v", err)
		return err
	}
	util.Infof("Bitget connected")
	return nil
}

// Subscribe subscribes to SPOT ticker for given instrument id (e.g., BTCUSDT)
func (c *BitgetClient) Subscribe(instId string) error {
	util.Debugf("Bitget subscribing to %s", instId)
	msg := map[string]interface{}{
		"op": "subscribe",
		"args": []map[string]string{{
			"channel":  "ticker",
			"instType": "SPOT",
			"instId":   instId,
		}},
	}
	if err := c.wsConn.WriteJSON(msg); err != nil {
		util.Errorf("Bitget subscribe error: %v", err)
		return err
	}
	return nil
}

// SubscribeBatch subscribes to multiple SPOT tickers in a single message
func (c *BitgetClient) SubscribeBatch(instIds []string) error {
	if len(instIds) == 0 {
		return nil
	}
	args := make([]map[string]string, 0, len(instIds))
	for _, id := range instIds {
		args = append(args, map[string]string{
			"channel":  "ticker",
			"instType": "SPOT",
			"instId":   id,
		})
	}
	msg := map[string]interface{}{
		"op":   "subscribe",
		"args": args,
	}
	util.Debugf("Bitget batch subscribe: %d symbols", len(instIds))
	if err := c.wsConn.WriteJSON(msg); err != nil {
		util.Errorf("Bitget subscribe batch error: %v", err)
		return err
	}
	return nil
}

func (c *BitgetClient) ReadLoop(exchangeName string) {
	defer func() {
		util.Infof("Bitget ReadLoop exiting")
		c.wsConn.Close()
	}()

	type bitgetArg struct {
		Channel  string `json:"channel"`
		InstType string `json:"instType"`
		InstId   string `json:"instId"`
	}
	type bitgetData struct {
		InstId  string `json:"instId"`
		LastPr  string `json:"lastPr"`
		Ts      string `json:"ts"`
		AskPr   string `json:"askPr"`
		BidPr   string `json:"bidPr"`
		BaseVol string `json:"baseVolume"`
	}
	type bitgetMsg struct {
		Action string       `json:"action"`
		Arg    *bitgetArg   `json:"arg"`
		Data   []bitgetData `json:"data"`
		Ts     int64        `json:"ts"`
		Code   int          `json:"code"`
		Event  string       `json:"event"`
		Msg    string       `json:"msg"`
	}

	// aggregate error logs (e.g., 30006 request too many)
	var agg30006 int64
	var agg30001 int64
	stopAgg := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c6 := atomic.SwapInt64(&agg30006, 0)
				c1 := atomic.SwapInt64(&agg30001, 0)
				if c6 > 0 || c1 > 0 {
					util.Infof("[WARN] Bitget errors aggregated last 5s: 30006=%d, 30001=%d", c6, c1)
				}
			case <-stopAgg:
				return
			}
		}
	}()

	for {
		_, message, err := c.wsConn.ReadMessage()
		if err != nil {
			util.Errorf("Bitget read error: %v", err)
			close(c.out)
			close(stopAgg)
			return
		}

		util.Debugf("Bitget raw message: %s", string(message))

		var m bitgetMsg
		if err := json.Unmarshal(message, &m); err != nil {
			util.Errorf("Bitget unmarshal error: %v", err)
			continue
		}

		// Handle error-typed messages
		if m.Code != 0 || m.Event == "error" {
			switch m.Code {
			case 30006:
				atomic.AddInt64(&agg30006, 1)
			case 30001:
				atomic.AddInt64(&agg30001, 1)
			default:
				util.Errorf("Bitget error msg code=%d event=%s msg=%s", m.Code, m.Event, m.Msg)
			}
			continue
		}
		if m.Arg == nil || m.Arg.Channel != "ticker" || strings.ToUpper(m.Arg.InstType) != "SPOT" {
			// ignore other channels/actions
			continue
		}
		if len(m.Data) == 0 {
			// snapshots may arrive with empty data on ack; skip
			continue
		}
		d := m.Data[0]
		if d.LastPr == "" || d.InstId == "" {
			continue
		}
		price, err := strconv.ParseFloat(d.LastPr, 64)
		if err != nil {
			util.Errorf("Bitget parse lastPr error: %v", err)
			continue
		}

		md := &protobuf.MarketData{
			Exchange:  exchangeName,
			Symbol:    d.InstId,
			Price:     price,
			Timestamp: time.Now().Unix(),
		}

		select {
		case c.out <- md:
		default:
			util.Debugf("Bitget drop message, no consumer")
		}
	}
}

func (c *BitgetClient) Close() {
	if c.wsConn != nil {
		util.Infof("closing Bitget client")
		c.wsConn.Close()
	}
}

func (c *BitgetClient) KeepAlive(interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		if err := c.wsConn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
			util.Errorf("Bitget ping error: %v", err)
			return
		}
		util.Debugf("Bitget ping sent")
	}
}
