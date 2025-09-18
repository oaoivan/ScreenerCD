package wsclient

import (
    "github.com/gorilla/websocket"
    "log"
    "sync"
)

type Client struct {
    conn *websocket.Conn
    mu   sync.Mutex
}

func NewClient(url string) (*Client, error) {
    conn, _, err := websocket.DefaultDialer.Dial(url, nil)
    if err != nil {
        return nil, err
    }
    return &Client{conn: conn}, nil
}

func (c *Client) SendMessage(message []byte) error {
    c.mu.Lock()
    defer c.mu.Unlock()
    return c.conn.WriteMessage(websocket.TextMessage, message)
}

func (c *Client) ReceiveMessage() ([]byte, error) {
    _, message, err := c.conn.ReadMessage()
    if err != nil {
        return nil, err
    }
    return message, nil
}

func (c *Client) Close() error {
    return c.conn.Close()
}

func (c *Client) IsConnected() bool {
    return c.conn != nil
}

func (c *Client) HandleError(err error) {
    if err != nil {
        log.Println("WebSocket error:", err)
    }
}