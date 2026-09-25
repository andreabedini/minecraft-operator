// Package msmp is a client for the Minecraft Server Management Protocol:
// JSON-RPC 2.0 over a WebSocket, available on Java Edition 1.21.9 and later.
// Parameters are positional. Notifications arrive on a channel.
package msmp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Player is a connected or listed player.
type Player struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// Version is the server's game version.
type Version struct {
	Name     string `json:"name"`
	Protocol int    `json:"protocol"`
}

// ServerState is the result of minecraft:server/status and the payload of
// the status heartbeat notification.
type ServerState struct {
	Started bool     `json:"started"`
	Players []Player `json:"players"`
	Version Version  `json:"version"`
}

// Notification is a server-initiated message.
type Notification struct {
	Method string
	Params json.RawMessage
}

// FirstParam decodes the first positional parameter into v.
func (n Notification) FirstParam(v any) error {
	var params []json.RawMessage
	if len(n.Params) == 0 {
		return errors.New("notification has no params")
	}
	if err := json.Unmarshal(n.Params, &params); err != nil {
		return err
	}
	if len(params) == 0 {
		return errors.New("notification has empty params")
	}
	return json.Unmarshal(params[0], v)
}

// Notification methods the operator cares about.
const (
	NotifyServerStarted  = "minecraft:notification/server/started"
	NotifyServerStopping = "minecraft:notification/server/stopping"
	NotifyServerSaving   = "minecraft:notification/server/saving"
	NotifyServerSaved    = "minecraft:notification/server/saved"
	NotifyServerStatus   = "minecraft:notification/server/status"
	NotifyPlayerJoined   = "minecraft:notification/players/joined"
	NotifyPlayerLeft     = "minecraft:notification/players/left"
	NotifyUpgradeStarted = "minecraft:notification/world/upgrade_started"
	NotifyUpgradeProg    = "minecraft:notification/world/upgrade_progress"
	NotifyUpgradeDone    = "minecraft:notification/world/upgrade_finished"
	NotifyUpgradeFailed  = "minecraft:notification/world/upgrade_failed"
)

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Dialer opens the raw connection to the management port. The operator
// passes the supervisor tunnel here.
type Dialer func(ctx context.Context) (net.Conn, error)

// Client is one management connection.
type Client struct {
	conn    *websocket.Conn
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan message
	notes   chan Notification
	done    chan struct{}
	err     error
}

// Dial connects and authenticates. The dialer's connection carries the
// WebSocket handshake; the URL host is nominal.
func Dial(ctx context.Context, dial Dialer, secret string) (*Client, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		// One connection per client; never pool it.
		DisableKeepAlives: true,
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+secret)
	conn, resp, err := websocket.Dial(ctx, "ws://management/", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: transport},
		HTTPHeader: header,
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("management protocol: unauthorized (check the secret)")
		}
		return nil, fmt.Errorf("management protocol: %w", err)
	}
	conn.SetReadLimit(16 << 20)
	c := &Client{
		conn:    conn,
		pending: map[int64]chan message{},
		notes:   make(chan Notification, 256),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *Client) readLoop() {
	defer close(c.done)
	defer close(c.notes)
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			c.fail(err)
			return
		}
		// Batches are arrays; single messages are objects.
		if len(data) > 0 && data[0] == '[' {
			var batch []message
			if err := json.Unmarshal(data, &batch); err != nil {
				continue
			}
			for _, m := range batch {
				c.dispatch(m)
			}
			continue
		}
		var m message
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		c.dispatch(m)
	}
}

func (c *Client) dispatch(m message) {
	if m.ID != nil {
		c.mu.Lock()
		ch, ok := c.pending[*m.ID]
		if ok {
			delete(c.pending, *m.ID)
		}
		c.mu.Unlock()
		if ok {
			ch <- m
		}
		return
	}
	if m.Method != "" {
		select {
		case c.notes <- Notification{Method: m.Method, Params: m.Params}:
		default:
			// A slow consumer loses notifications rather than blocking reads.
		}
	}
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
	}
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
}

// Call invokes a method with positional params and decodes the result.
func (c *Client) Call(ctx context.Context, method string, params []any, result any) error {
	id := c.nextID.Add(1)
	if params == nil {
		params = []any{}
	}
	payload, err := json.Marshal(request{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	ch := make(chan message, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.conn.Write(ctx, websocket.MessageText, payload); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case m, ok := <-ch:
		if !ok {
			return c.Err()
		}
		if m.Error != nil {
			return m.Error
		}
		if result == nil || len(m.Result) == 0 {
			return nil
		}
		return json.Unmarshal(m.Result, result)
	}
}

// Notifications yields server notifications until the connection ends.
func (c *Client) Notifications() <-chan Notification { return c.notes }

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is the error that ended the connection, if any.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection.
func (c *Client) Close() error {
	err := c.conn.Close(websocket.StatusNormalClosure, "bye")
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
	}
	return err
}

// Status calls minecraft:server/status.
func (c *Client) Status(ctx context.Context) (*ServerState, error) {
	var st ServerState
	if err := c.Call(ctx, "minecraft:server/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ProtocolVersion reads info.version from rpc.discover.
func (c *Client) ProtocolVersion(ctx context.Context) (string, error) {
	var schema struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := c.Call(ctx, "rpc.discover", nil, &schema); err != nil {
		return "", err
	}
	return schema.Info.Version, nil
}

// Save calls minecraft:server/save.
func (c *Client) Save(ctx context.Context, flush bool) error {
	return c.Call(ctx, "minecraft:server/save", []any{flush}, nil)
}

// Stop calls minecraft:server/stop.
func (c *Client) Stop(ctx context.Context) error {
	return c.Call(ctx, "minecraft:server/stop", nil, nil)
}

// SystemMessage broadcasts a literal chat message.
func (c *Client) SystemMessage(ctx context.Context, text string, overlay bool) error {
	msg := map[string]any{
		"message": map[string]any{"literal": text},
		"overlay": overlay,
	}
	return c.Call(ctx, "minecraft:server/system_message", []any{msg}, nil)
}
