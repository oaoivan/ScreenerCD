package exchange

import (
    "github.com/gorilla/websocket"
    "sync"
)

type Connector struct {
    mu       sync.Mutex
    clients  map[*websocket.Conn]bool
    endpoint string
}

func NewConnector(endpoint string) *Connector {
    return &Connector{
        clients:  make(map[*websocket.Conn]bool),
        endpoint: endpoint,
    }
}

func (c *Connector) Subscribe(conn *websocket.Conn) {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.clients[conn] = true
}

func (c *Connector) Unsubscribe(conn *websocket.Conn) {
    c.mu.Lock()
    defer c.mu.Unlock()
    delete(c.clients, conn)
}

func (c *Connector) Broadcast(message []byte) {
    c.mu.Lock()
    defer c.mu.Unlock()
    for conn := range c.clients {
        err := conn.WriteMessage(websocket.TextMessage, message)
        if err != nil {
            conn.Close()
            delete(c.clients, conn)
        }
    }
}

func (c *Connector) Connect() error {
    conn, _, err := websocket.DefaultDialer.Dial(c.endpoint, nil)
    if err != nil {
        return err
    }
    go c.handleMessages(conn)
    return nil
}

func (c *Connector) handleMessages(conn *websocket.Conn) {
    defer conn.Close()
    for {
        _, message, err := conn.ReadMessage()
        if err != nil {
            break
        }
        c.Broadcast(message)
    }
}