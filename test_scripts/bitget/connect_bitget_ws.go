package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/gorilla/websocket"
)

// Minimal Bitget public spot WS smoke test
// Connects, subscribes to ticker channel for a few symbols and logs incoming messages.
func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	urls := []string{
		"wss://ws.bitget.com/spot/v1/stream",
		"wss://ws.bitget.com/v2/ws/public",
	}
	for ui, url := range urls {
		log.Printf("[INFO] connecting Bitget WS[%d]: %s", ui, url)
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			log.Printf("[ERR ] dial[%d]: %v", ui, err)
			continue
		}
		log.Printf("[INFO] connected[%d]", ui)
		runOnce(conn)
		conn.Close()
	}
	log.Printf("[INFO] done all urls")
}

func runOnce(conn *websocket.Conn) {

	// Try multiple subscription formats (Bitget API variants) to find correct one
	subs := []map[string]interface{}{
		{ // v1 style with instType
			"op": "subscribe",
			"args": []map[string]string{
				{"instType": "SPOT", "channel": "ticker", "instId": "BTCUSDT"},
				{"instType": "SPOT", "channel": "ticker", "instId": "ETHUSDT"},
			},
		},
		{ // without instType
			"op": "subscribe",
			"args": []map[string]string{
				{"channel": "ticker", "instId": "BTCUSDT"},
				{"channel": "ticker", "instId": "ETHUSDT"},
			},
		},
		{ // with _SPBL suffix used by some Bitget APIs for spot
			"op": "subscribe",
			"args": []map[string]string{
				{"instType": "SPOT", "channel": "ticker", "instId": "BTCUSDT_SPBL"},
				{"instType": "SPOT", "channel": "ticker", "instId": "ETHUSDT_SPBL"},
			},
		},
		{ // _SPBL without instType
			"op": "subscribe",
			"args": []map[string]string{
				{"channel": "ticker", "instId": "BTCUSDT_SPBL"},
				{"channel": "ticker", "instId": "ETHUSDT_SPBL"},
			},
		},
		{ // channel with spot/ prefix
			"op": "subscribe",
			"args": []map[string]string{
				{"channel": "spot/ticker", "instId": "BTCUSDT"},
				{"channel": "spot/ticker", "instId": "ETHUSDT"},
			},
		},
	}
	for idx, sub := range subs {
		if err := conn.WriteJSON(sub); err != nil {
			log.Printf("[ERR ] subscribe[%d]: %v", idx, err)
		} else {
			b, _ := json.Marshal(sub)
			log.Printf("[INFO] subscribe[%d] sent: %s", idx, string(b))
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Keepalive via ping frames
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
					log.Printf("[ERR ] ping: %v", err)
					return
				}
				log.Printf("[DBG ] ping sent")
			}
		}
	}()

	// Reader
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			mt, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("[ERR ] read: %v", err)
				return
			}
			if mt == websocket.PongMessage {
				log.Printf("[DBG ] pong recv")
				continue
			}
			// Try to pretty print JSON, fallback to raw
			var js map[string]interface{}
			if err := json.Unmarshal(message, &js); err == nil {
				b, _ := json.Marshal(js)
				log.Printf("[RAW ] %s", string(b))
			} else {
				log.Printf("[RAW ] %s", string(message))
			}
		}
	}()

	// Wait for Ctrl+C or 30s timeout
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	select {
	case <-sig:
		log.Printf("[INFO] interrupt")
	case <-time.After(30 * time.Second):
		log.Printf("[INFO] timeout reached, closing")
	case <-done:
		log.Printf("[INFO] reader exited")
	}

	// Graceful close
	if err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye")); err != nil {
		log.Printf("[ERR ] close: %v", err)
	}
	// Wait a bit
	time.Sleep(500 * time.Millisecond)
	fmt.Println("[INFO] one connection done")
}
