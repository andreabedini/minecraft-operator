// Command fakeserver imitates a Minecraft Java server from the outside, for
// end-to-end tests without Mojang downloads. Installed as /usr/bin/java in
// the e2e image, it ignores its JVM arguments, reads server.properties from
// the working directory, listens on the game port, serves the management
// protocol on the configured loopback port, prints the usual startup lines,
// and reacts to "stop" and "save-all flush" on stdin. Two seconds after
// starting it simulates a player joining, so status and notifications have
// something to show.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type player struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type state struct {
	mu      sync.Mutex
	started bool
	players []player
	subs    map[*websocket.Conn]struct{}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "publish" {
		publish(os.Args[2:])
		return
	}
	if _, err := os.Stat("eula.txt"); err != nil {
		logLine("WARN", "You need to agree to the EULA in order to run the server. Go to eula.txt for more info.")
		os.Exit(1)
	}
	props := readProperties("server.properties")
	gamePort := atoi(props["server-port"], 25565)
	mgmtEnabled := props["management-server-enabled"] == "true"
	mgmtHost := props["management-server-host"]
	if mgmtHost == "" {
		mgmtHost = "localhost"
	}
	mgmtPort := atoi(props["management-server-port"], 25585)
	secret := props["management-server-secret"]

	st := &state{subs: map[*websocket.Conn]struct{}{}}
	logLine("INFO", "Starting minecraft server version fake-1.0")
	logLine("INFO", "Loading properties")

	if mgmtEnabled {
		ln, err := net.Listen("tcp", net.JoinHostPort(mgmtHost, strconv.Itoa(mgmtPort)))
		if err != nil {
			logLine("ERROR", "management server: "+err.Error())
			os.Exit(1)
		}
		srv := &http.Server{Handler: st.managementHandler(secret), ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		logLine("INFO", fmt.Sprintf("Management server started on %s", ln.Addr()))
	}

	game, err := net.Listen("tcp", fmt.Sprintf(":%d", gamePort))
	if err != nil {
		logLine("ERROR", "game port: "+err.Error())
		os.Exit(1)
	}
	go func() {
		for {
			c, err := game.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	logLine("INFO", fmt.Sprintf("Starting Minecraft server on *:%d", gamePort))
	time.Sleep(500 * time.Millisecond)
	st.mu.Lock()
	st.started = true
	st.mu.Unlock()
	logLine("INFO", `Done (0.5s)! For help, type "help"`)
	st.notify("minecraft:notification/server/started", nil)

	go func() {
		time.Sleep(2 * time.Second)
		st.join(player{ID: "069a79f4-44e9-4726-a5be-fca90e38aaf5", Name: "steve"})
	}()

	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "stop":
			logLine("INFO", "Stopping the server")
			st.notify("minecraft:notification/server/stopping", nil)
			logLine("INFO", "Saving players")
			logLine("INFO", "Saving worlds")
			os.Exit(0)
		case line == "save-off":
			logLine("INFO", "Automatic saving is now disabled")
		case line == "save-on":
			logLine("INFO", "Automatic saving is now enabled")
		case strings.HasPrefix(line, "save-all"):
			st.notify("minecraft:notification/server/saving", nil)
			logLine("INFO", "Saved the game")
			st.notify("minecraft:notification/server/saved", nil)
		case strings.HasPrefix(line, "fakejoin "):
			st.join(player{ID: "00000000-0000-0000-0000-00000000" + fmt.Sprintf("%04d", len(line)), Name: strings.TrimPrefix(line, "fakejoin ")})
		case strings.HasPrefix(line, "fakeleave "):
			st.leave(strings.TrimPrefix(line, "fakeleave "))
		case line == "":
		default:
			logLine("INFO", "Unknown command: "+line)
		}
	}
	// stdin closed: the supervisor went away; behave like a real server and
	// keep running until signalled.
	select {}
}

func (s *state) join(p player) {
	s.mu.Lock()
	s.players = append(s.players, p)
	s.mu.Unlock()
	logLine("INFO", p.Name+" joined the game")
	s.notify("minecraft:notification/players/joined", []any{p})
}

func (s *state) leave(name string) {
	s.mu.Lock()
	var left *player
	kept := s.players[:0]
	for _, p := range s.players {
		if p.Name == name && left == nil {
			pp := p
			left = &pp
			continue
		}
		kept = append(kept, p)
	}
	s.players = kept
	s.mu.Unlock()
	if left != nil {
		logLine("INFO", name+" left the game")
		s.notify("minecraft:notification/players/left", []any{*left})
	}
}

func (s *state) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	players := s.players
	if players == nil {
		players = []player{}
	}
	return map[string]any{
		"started": s.started,
		"players": players,
		"version": map[string]any{"name": "fake-1.0", "protocol": 775},
	}
}

func (s *state) notify(method string, params []any) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, _ := json.Marshal(msg)
	s.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(s.subs))
	for c := range s.subs {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.Write(ctx, websocket.MessageText, data)
		cancel()
	}
}

func (s *state) managementHandler(secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret != "" && r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.subs[conn] = struct{}{}
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.subs, conn)
			s.mu.Unlock()
			_ = conn.CloseNow()
		}()
		ctx := r.Context()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var req struct {
				ID     *json.RawMessage  `json:"id"`
				Method string            `json:"method"`
				Params []json.RawMessage `json:"params"`
			}
			if json.Unmarshal(data, &req) != nil || req.ID == nil {
				continue
			}
			var result any
			var rpcErr map[string]any
			switch req.Method {
			case "rpc.discover":
				result = map[string]any{"openrpc": "1.3.2", "info": map[string]any{"title": "Fake Minecraft Server Management API", "version": "3.1.0"}, "methods": []any{}}
			case "minecraft:server/status":
				result = s.snapshot()
			case "minecraft:server/save":
				logLine("INFO", "Saved the game")
				s.notify("minecraft:notification/server/saved", nil)
				result = true
			case "minecraft:server/stop":
				result = true
				go func() {
					time.Sleep(200 * time.Millisecond)
					logLine("INFO", "Stopping the server")
					os.Exit(0)
				}()
			case "minecraft:server/system_message":
				result = true
			case "minecraft:players":
				result = s.snapshot()["players"]
			default:
				rpcErr = map[string]any{"code": -32601, "message": "Method not found", "data": "Method not found: " + req.Method}
			}
			resp := map[string]any{"jsonrpc": "2.0", "id": *req.ID}
			if rpcErr != nil {
				resp["error"] = rpcErr
			} else {
				resp["result"] = result
			}
			out, _ := json.Marshal(resp)
			if err := conn.Write(ctx, websocket.MessageText, out); err != nil {
				return
			}
		}
	})
}

func readProperties(path string) map[string]string {
	props := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return props
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			props[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return props
}

func atoi(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func logLine(level, msg string) {
	fmt.Printf("[%s] [Server thread/%s]: %s\n", time.Now().Format("15:04:05"), level, msg)
}
