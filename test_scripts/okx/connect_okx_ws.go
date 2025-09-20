package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yourusername/screner/internal/util"
)

// Minimal standalone OKX WS connector for public spot tickers
// - Connects to wss://ws.okx.com:8443/ws/v5/public
// - Subscribes to tickers for the common symbols list (Temp/all_contracts_merged_reformatted.json)
// - Uses batch subscribe (args: [{channel:"tickers", instId:"BTC-USDT"}, ...])
// - Parses messages and logs price updates count per 5s

const okxWSURL = "wss://ws.okx.com:8443/ws/v5/public"

func bybitToOKX(bybit string) string {
	if len(bybit) < 5 {
		return strings.ToUpper(bybit)
	}
	s := strings.ToUpper(bybit)
	suffixes := []string{"USDT", "USDC", "DAI", "BUSD"}
	for _, suf := range suffixes {
		if len(s) > len(suf) && strings.HasSuffix(s, suf) {
			base := s[:len(s)-len(suf)]
			return base + "-" + suf
		}
	}
	// default fallback: insert hyphen before last 4
	if len(s) > 4 {
		return s[:len(s)-4] + "-" + s[len(s)-4:]
	}
	return s
}

func subscribeBatch(conn *websocket.Conn, instIds []string) error {
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
	return conn.WriteJSON(msg)
}

func main() {
	batchSize := flag.Int("batch", 50, "batch size for OKX subscribe")
	pauseMs := flag.Int("pause", 500, "pause between batches, ms")
	flag.Parse()

	// Load common symbols (Bybit format: BTCUSDT)
	symbols, err := util.LoadSymbolsFromFile("Temp/all_contracts_merged_reformatted.json")
	if err != nil {
		util.Fatalf("failed to load symbols: %v", err)
	}
	util.Infof("OKX test: loaded %d symbols", len(symbols))

	// Connect WS
	util.Infof("connecting to OKX WS %s", okxWSURL)
	conn, _, err := websocket.DefaultDialer.Dial(okxWSURL, nil)
	if err != nil {
		util.Fatalf("OKX connect error: %v", err)
	}
	defer conn.Close()
	util.Infof("OKX connected")

	// Graceful shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	stop := make(chan struct{})

	// KeepAlive (ping)
	go func() {
		interval := 25 * time.Second
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
					util.Errorf("OKX ping error: %v", err)
					return
				}
				util.Debugf("OKX ping sent")
			case <-stop:
				return
			}
		}
	}()

	// Subscribe in batches
	batch := make([]string, 0, *batchSize)
	total := 0
	for _, s := range symbols {
		instId := bybitToOKX(s)
		batch = append(batch, instId)
		total++
		if len(batch) >= *batchSize {
			if err := subscribeBatch(conn, batch); err != nil {
				util.Errorf("OKX subscribe batch error (size=%d): %v", len(batch), err)
			} else {
				util.Infof("OKX subscribed %d symbols (batch), pausing %dms", total, *pauseMs)
			}
			batch = batch[:0]
			time.Sleep(time.Duration(*pauseMs) * time.Millisecond)
		}
	}
	if len(batch) > 0 {
		if err := subscribeBatch(conn, batch); err != nil {
			util.Errorf("OKX subscribe batch error (size=%d): %v", len(batch), err)
		} else {
			util.Infof("OKX subscribed %d symbols (batch), pausing %dms", total, *pauseMs)
		}
		batch = batch[:0]
	}

	// Reader
	type okxArg struct {
		Channel string `json:"channel"`
		InstId  string `json:"instId"`
	}
	type okxTicker struct {
		InstId string `json:"instId"`
		Last   string `json:"last"`
		Ts     string `json:"ts"`
		BidPx  string `json:"bidPx"`
		AskPx  string `json:"askPx"`
	}
	type okxMsg struct {
		Arg   *okxArg     `json:"arg"`
		Data  []okxTicker `json:"data"`
		Event string      `json:"event"`
		Code  string      `json:"code"`
		Msg   string      `json:"msg"`
	}

	var received int64
	var aggErr int64

	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				util.Errorf("OKX read error: %v", err)
				close(stop)
				return
			}
			var m okxMsg
			if err := json.Unmarshal(raw, &m); err != nil {
				util.Errorf("OKX unmarshal error: %v", err)
				continue
			}
			if m.Event == "error" || (m.Code != "" && m.Code != "0") {
				atomic.AddInt64(&aggErr, 1)
				continue
			}
			if m.Arg == nil || m.Arg.Channel != "tickers" || len(m.Data) == 0 {
				continue
			}
			atomic.AddInt64(&received, int64(len(m.Data)))
		}
	}()

	// Metrics every 5s
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		var prev int64
		for {
			select {
			case <-t.C:
				cur := atomic.LoadInt64(&received)
				delta := cur - prev
				prev = cur
				e := atomic.SwapInt64(&aggErr, 0)
				util.Infof("OKX metrics: msgs/5s=%d, errors=%d", delta, e)
			case <-stop:
				return
			}
		}
	}()

	// Wait
	<-sig
	close(stop)
	fmt.Println("OKX standalone stopped")
}
