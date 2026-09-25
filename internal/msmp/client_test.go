package msmp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeServer speaks enough of the protocol to exercise the client.
func fakeServer(t *testing.T, secret string) (*httptest.Server, chan<- string) {
	t.Helper()
	push := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		go func() {
			for raw := range push {
				if raw == "CLOSE" {
					_ = conn.Close(websocket.StatusGoingAway, "bye")
					return
				}
				_ = conn.Write(ctx, websocket.MessageText, []byte(raw))
			}
		}()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
				Params []any  `json:"params"`
			}
			if err := json.Unmarshal(data, &req); err != nil {
				continue
			}
			var reply string
			switch req.Method {
			case "rpc.discover":
				reply = `{"jsonrpc":"2.0","id":%d,"result":{"openrpc":"1.3.2","info":{"title":"Minecraft Server Management API","version":"3.1.0"},"methods":[]}}`
			case "minecraft:server/status":
				reply = `{"jsonrpc":"2.0","id":%d,"result":{"started":true,"players":[{"id":"853c80ef-3c37-49fd-aa49-938b674adae6","name":"jeb_"}],"version":{"name":"26.3","protocol":775}}}`
			case "minecraft:server/save":
				if len(req.Params) != 1 || req.Params[0] != true {
					reply = `{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"Invalid params"}}`
				} else {
					reply = `{"jsonrpc":"2.0","id":%d,"result":true}`
				}
			case "minecraft:server/system_message":
				reply = `{"jsonrpc":"2.0","id":%d,"result":true}`
			default:
				reply = `{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"Method not found","data":"Method not found: ` + req.Method + `"}}`
			}
			_ = conn.Write(ctx, websocket.MessageText, []byte(strings.Replace(reply, "%d", jsonInt(req.ID), 1)))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, push
}

func jsonInt(i int64) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func dialerFor(srv *httptest.Server) Dialer {
	addr := strings.TrimPrefix(srv.URL, "http://")
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

func TestClientCallsAndNotifications(t *testing.T) {
	srv, push := fakeServer(t, "s3cret")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := Dial(ctx, dialerFor(srv), "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	v, err := c.ProtocolVersion(ctx)
	if err != nil || v != "3.1.0" {
		t.Errorf("protocol version = %q, %v", v, err)
	}
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Started || len(st.Players) != 1 || st.Players[0].Name != "jeb_" || st.Version.Name != "26.3" {
		t.Errorf("status = %+v", st)
	}
	if err := c.Save(ctx, true); err != nil {
		t.Errorf("save: %v", err)
	}
	if err := c.SystemMessage(ctx, "hello", false); err != nil {
		t.Errorf("system message: %v", err)
	}
	err = c.Call(ctx, "minecraft:foo/bar", nil, nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Errorf("unknown method: %v", err)
	}

	push <- `{"jsonrpc":"2.0","method":"minecraft:notification/players/joined","params":[{"id":"853c80ef-3c37-49fd-aa49-938b674adae6","name":"jeb_"}]}`
	push <- `{"jsonrpc":"2.0","method":"minecraft:notification/server/started"}`
	push <- `[{"jsonrpc":"2.0","method":"minecraft:notification/server/status","params":[{"started":true,"players":[],"version":{"name":"26.3","protocol":775}}]}]`

	select {
	case n := <-c.Notifications():
		if n.Method != NotifyPlayerJoined {
			t.Fatalf("first notification = %s", n.Method)
		}
		var p Player
		if err := n.FirstParam(&p); err != nil || p.Name != "jeb_" {
			t.Errorf("player = %+v, %v", p, err)
		}
	case <-ctx.Done():
		t.Fatal("no notification")
	}
	n := <-c.Notifications()
	if n.Method != NotifyServerStarted {
		t.Errorf("second notification = %s", n.Method)
	}
	if err := n.FirstParam(&struct{}{}); err == nil {
		t.Error("expected an error decoding params of a bare notification")
	}
	n = <-c.Notifications()
	var state ServerState
	if n.Method != NotifyServerStatus || n.FirstParam(&state) != nil || !state.Started {
		t.Errorf("batched heartbeat = %+v", n)
	}
}

func TestClientRejectsBadSecret(t *testing.T) {
	srv, _ := fakeServer(t, "right")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Dial(ctx, dialerFor(srv), "wrong")
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("bad secret: %v", err)
	}
}

func TestClientDoneOnServerClose(t *testing.T) {
	srv, push := fakeServer(t, "s")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, dialerFor(srv), "s")
	if err != nil {
		t.Fatal(err)
	}
	push <- "CLOSE"
	select {
	case <-c.Done():
	case <-ctx.Done():
		t.Fatal("Done not closed after the server dropped the connection")
	}
	if err := c.Call(ctx, "minecraft:server/status", nil, nil); err == nil {
		t.Error("call after close succeeded")
	}
}
