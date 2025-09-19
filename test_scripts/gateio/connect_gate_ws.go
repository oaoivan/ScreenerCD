package main


import (
	"context"
	"flag"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	util "github.com/yourusername/screner/internal/util"
)

// This is a standalone smoke test that only establishes a WS connection to Gate.io v4
// without any subscriptions. It logs every step and exits gracefully.
func main() {
	endpoint := flag.String("ws", "wss://api.gateio.ws/ws/v4/", "Gate.io WS v4 endpoint")
	duration := flag.Duration("t", 10*time.Second, "How long to keep the connection open for test")
	flag.Parse()

	util.SetLevel("debug")
	util.Infof("Gate.io WS connect smoke test: endpoint=%s, duration=%s", *endpoint, duration.String())

	u, err := url.Parse(*endpoint)
	if err != nil {
		util.Fatalf("invalid ws url: %v", err)
	}

	// headers if needed later
	headers := http.Header{}

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		EnableCompression: true,
	}

	util.Infof("dialing %s", u.String())
	conn, resp, err := dialer.Dial(u.String(), headers)
	if err != nil {
		status := 0
		if resp != nil { status = resp.StatusCode }
		util.Fatalf("dial error: %v, status=%d", err, status)
	}
	defer conn.Close()

	util.Infof("connected to %s", u.String())

	// context with timeout to close connection after duration
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	// SIGINT/SIGTERM handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	// Reader goroutine: read and log any server messages (e.g., pongs)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			msgType, message, err := conn.ReadMessage()
			if err != nil {
				util.Errorf("read error: %v", err)
				return
			}
			util.Debugf("incoming frame type=%d len=%d", msgType, len(message))
		}
	}()

	// Periodic ping to keep the connection alive
	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()

loop:
	for {
		select {
		case <-ctx.Done():
			util.Infof("time is up; closing connection")
			break loop
		case sig := <-sigCh:
			util.Infof("received signal: %v; closing connection", sig)
			break loop
		case <-pingTicker.C:
			if err := conn.WriteMessage(websocket.PingMessage, []byte("ping")); err != nil {
				util.Errorf("ping error: %v", err)
				break loop
			}
			util.Debugf("ping sent")
		}
	}

	// close gracefully
	_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
	select {
	case <-readDone:
		util.Infof("reader stopped")
	case <-time.After(2 * time.Second):
		util.Debugf("closing without reader ack")
	}
	util.Infof("done")
}
