package exchange

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yourusername/screner/internal/util"
	"github.com/yourusername/screner/pkg/protobuf"
)

// OkxClient implements WS client for OKX public v5 tickers
type OkxClient struct {
	wsConn *websocket.Conn
	url    string
	out    chan *protobuf.MarketData
}

func NewOkxClient(url string) *OkxClient {
	util.Debugf("creating OKX client for url=%s", url)
	return &OkxClient{url: url, out: make(chan *protobuf.MarketData)}
}

func (c *OkxClient) Out() <-chan *protobuf.MarketData { return c.out }

func (c *OkxClient) Connect() error {
	var err error
	util.Infof("connecting to OKX WS %s", c.url)
	c.wsConn, _, err = websocket.DefaultDialer.Dial(c.url, nil)
	if err != nil {
		util.Errorf("OKX connect error: %v", err)
		return err
	}
	util.Infof("OKX connected")
	return nil
}

// SubscribeBatch subscribes to OKX tickers for given instIds (e.g., BTC-USDT)
func (c *OkxClient) SubscribeBatch(instIds []string) error {
	if len(instIds) == 0 {
		return nil
	}
	args := make([]map[string]string, 0, len(instIds))
	for _, id := range instIds {
		args = append(args, map[string]string{
			"channel": "tickers",
			"instId":  id,
		})
	}
	msg := map[string]interface{}{
		"op":   "subscribe",
		"args": args,
	}
	util.Debugf("OKX batch subscribe: %d symbols", len(instIds))
	if err := c.wsConn.WriteJSON(msg); err != nil {
		util.Errorf("OKX subscribe batch error: %v", err)
		return err
	}
	return nil
}

func (c *OkxClient) ReadLoop(exchangeName string) {
	defer func() {
		util.Infof("OKX ReadLoop exiting")
		c.wsConn.Close()
	}()

	type okxArg struct {
		Channel string `json:"channel"`
		InstId  string `json:"instId"`
	}
	type okxData struct {
		InstId string `json:"instId"`
		Last   string `json:"last"`
		Ts     string `json:"ts"`
	}
	type okxMsg struct {
		Arg   *okxArg   `json:"arg"`
		Data  []okxData `json:"data"`
		Event string    `json:"event"`
		Code  string    `json:"code"`
		Msg   string    `json:"msg"`
	}

	for {
		_, message, err := c.wsConn.ReadMessage()
		if err != nil {
			util.Errorf("OKX read error: %v", err)
			close(c.out)
			return
		}

		// util.Debugf("OKX raw: %s", string(message))
		var m okxMsg
		if err := json.Unmarshal(message, &m); err != nil {
			// not all frames are JSON ticker payloads (e.g., pongs); ignore parse errors
			continue
		}
		if m.Event == "error" || (m.Code != "" && m.Code != "0") {
			util.Errorf("OKX error event code=%s msg=%s", m.Code, m.Msg)
			continue
		}
		if m.Arg == nil || m.Arg.Channel != "tickers" || len(m.Data) == 0 {
			// acks or other channels
			continue
		}
		d := m.Data[0]
		if d.InstId == "" || d.Last == "" {
			continue
		}
		price, err := strconv.ParseFloat(d.Last, 64)
		if err != nil {
			util.Errorf("OKX parse last error: %v", err)
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
			util.Debugf("OKX drop message, no consumer")
		}
	}
}

func (c *OkxClient) Close() {
	if c.wsConn != nil {
		util.Infof("closing OKX client")
		c.wsConn.Close()
	}
}

func (c *OkxClient) KeepAlive(interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		if err := c.wsConn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
			util.Errorf("OKX ping error: %v", err)
			return
		}
		util.Debugf("OKX ping sent")
	}
}
