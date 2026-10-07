// Synthetic Docker endpoint and disposable database seed/audit for the browser
// integration harness. This is a test program, never linked into agentbox.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentbox/internal/password"
	"agentbox/internal/store"
	"github.com/docker/docker/pkg/stdcopy"
)

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
func output(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	must(json.NewEncoder(w).Encode(value))
}
func write(path string, v any) {
	b, err := json.Marshal(v)
	must(err)
	must(os.WriteFile(path, b, 0600))
}
func seed(dir, listen string) {
	must(os.MkdirAll(dir, 0700))
	if _, err := os.Stat(filepath.Join(dir, "state.db")); !os.IsNotExist(err) {
		log.Fatal("seed requires a new, empty fixture database")
	}
	must(os.WriteFile(filepath.Join(dir, ".chat-integration-fixture"), []byte("synthetic-only\n"), 0600))
	st, err := store.Open(filepath.Join(dir, "state.db"))
	must(err)
	defer st.Close()
	for _, name := range []string{"alice", "bob"} {
		must(st.CreateUser(store.User{Name: name, Role: store.RoleUser, PassHash: password.Hash("synthetic-password")}))
		_, err = st.Grant(name, 10000, "fixture-"+name, "synthetic grant", "fixture")
		must(err)
	}
	for _, id := range []string{"space-a", "space-b", "space-c"} {
		user := "alice"
		if id == "space-c" {
			user = "bob"
		}
		sess := store.Session{ID: id, User: user, Name: id, Agent: "codex", AccountID: "fixture", DefaultModel: "gpt-5.5", Status: store.StatusRunning, ContainerID: id}
		must(st.Put(sess))
		base := filepath.Join(dir, "users", user, "sessions", id)
		for _, p := range []string{"home", "workspace", "chats"} {
			must(os.MkdirAll(filepath.Join(base, p), 0700))
		}
		for _, tid := range []string{"thread-one", "thread-two"} {
			data := fmt.Sprintf("{\"kind\":\"user\",\"text\":\"Synthetic prior turn\",\"ts\":\"2026-10-05T00:00:00Z\"}\n{\"kind\":\"title\",\"text\":%q,\"ts\":\"2026-10-05T00:00:00Z\"}\n", tid)
			must(os.WriteFile(filepath.Join(base, "chats", tid+".jsonl"), []byte(data), 0600))
		}
		must(os.WriteFile(filepath.Join(base, "chats", "active"), []byte("thread-one"), 0600))
	}
	write(filepath.Join(dir, "config.json"), map[string]any{
		"listen": listen, "data_dir": dir, "auth_token": "synthetic-unused-admin-password", "idle_timeout_min": 0, "timezone": "UTC",
		"tunnel": map[string]any{"enabled": false, "transparent": false}, "proxy_bridge": map[string]any{"bind": "127.0.0.1:0"},
		"accounts": []any{map[string]any{"id": "fixture", "type": "codex", "label": "Synthetic only", "env": map[string]string{"OPENAI_BASE_URL": "http://127.0.0.1:1", "OPENAI_API_KEY": "synthetic-not-a-key"}}},
		"models":   map[string]any{"codex": []any{map[string]string{"id": "gpt-5.5", "label": "Synthetic Codex"}}},
		"pricing":  map[string]any{"codex": map[string]int{"input": 1}},
	})
}
func dbFor(dir string, writable bool) *sql.DB {
	b, err := os.ReadFile(filepath.Join(dir, ".chat-integration-fixture"))
	must(err)
	if string(b) != "synthetic-only\n" {
		log.Fatal("not a fixture directory")
	}
	mode := "ro"
	if writable {
		mode = "rw"
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db")+"?mode="+mode+"&_pragma=busy_timeout(5000)")
	must(err)
	return db
}
func query(db *sql.DB, q string) []map[string]any {
	rows, err := db.Query(q)
	must(err)
	defer rows.Close()
	cols, err := rows.Columns()
	must(err)
	out := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(cols))
		ptr := make([]any, len(cols))
		for i := range values {
			ptr[i] = &values[i]
		}
		must(rows.Scan(ptr...))
		row := map[string]any{}
		for i, k := range cols {
			row[k] = values[i]
		}
		out = append(out, row)
	}
	must(rows.Err())
	return out
}
func audit(dir string) any {
	db := dbFor(dir, false)
	defer db.Close()
	return map[string]any{
		"receipts": query(db, "SELECT user,session_id,thread_id,request_id,turn_id,state,error_code,revision FROM chat_requests ORDER BY created_at"),
		"usage":    query(db, "SELECT id,user,session_id,thread_id,turn_id,kind,cost_micro_usd FROM usage_events ORDER BY id"),
		"ledger":   query(db, "SELECT user,ref,reason,delta_micro_usd,balance_after FROM credit_ledger ORDER BY id"),
		"quotas":   query(db, "SELECT user,balance_micro_usd FROM quotas ORDER BY user"),
	}
}

type invocation struct {
	ID      string `json:"id"`
	Session string `json:"session"`
	Prompt  string `json:"prompt"`
	Done    bool   `json:"done"`
	Resume  bool   `json:"resume"`
	conn    net.Conn
	release chan string
}
type engine struct {
	mu            sync.Mutex
	dir, token    string
	serial        int
	commands      map[string]struct{ Session, Command string }
	turns         []*invocation
	unexpected    []string
	interruptions int
	connections   map[net.Conn]bool
}

func (e *engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	parts := strings.Split(path, "/")
	if strings.HasPrefix(path, "/control/") {
		if r.Header.Get("Authorization") != "Bearer "+e.token {
			w.WriteHeader(403)
			return
		}
		switch path {
		case "/control/state":
			e.mu.Lock()
			defer e.mu.Unlock()
			output(w, map[string]any{"turns": e.turns, "interruptions": e.interruptions, "unexpected": e.unexpected})
		case "/control/audit":
			output(w, audit(e.dir))
		case "/control/finish":
			var body struct{ Prompt, Output string }
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				w.WriteHeader(400)
				return
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			for _, turn := range e.turns {
				if turn.Prompt == body.Prompt && !turn.Done {
					turn.Done = true
					turn.release <- body.Output
					output(w, map[string]bool{"ok": true})
					return
				}
			}
			w.WriteHeader(404)
		case "/control/settlement-fault":
			var body struct{ Enabled bool }
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				w.WriteHeader(400)
				return
			}
			db := dbFor(e.dir, true)
			defer db.Close()
			q := "DROP TRIGGER IF EXISTS fixture_settlement_failure"
			if body.Enabled {
				q = "CREATE TRIGGER fixture_settlement_failure BEFORE INSERT ON usage_events BEGIN SELECT RAISE(ABORT, 'synthetic settlement failure'); END"
			}
			_, err := db.Exec(q)
			must(err)
			output(w, map[string]bool{"ok": true})
		default:
			w.WriteHeader(404)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(path, "/_ping"):
		w.Header().Set("API-Version", "1.45")
		_, _ = w.Write([]byte("OK"))
	case strings.HasSuffix(path, "/version"):
		output(w, map[string]string{"Version": "synthetic", "ApiVersion": "1.45"})
	case strings.Contains(path, "/containers/") && strings.HasSuffix(path, "/json"):
		output(w, map[string]any{"Id": parts[len(parts)-2], "State": map[string]bool{"Running": true}, "Mounts": []any{map[string]string{"Destination": "/shared"}}})
	case strings.Contains(path, "/containers/") && strings.HasSuffix(path, "/exec"):
		var body struct{ Cmd []string }
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(400)
			return
		}
		e.mu.Lock()
		e.serial++
		id := fmt.Sprintf("exec-%d", e.serial)
		e.commands[id] = struct{ Session, Command string }{parts[len(parts)-2], strings.Join(body.Cmd, " ")}
		e.mu.Unlock()
		w.WriteHeader(201)
		output(w, map[string]string{"Id": id})
	case strings.Contains(path, "/exec/") && strings.HasSuffix(path, "/start"):
		id := parts[len(parts)-2]
		e.mu.Lock()
		cmd, ok := e.commands[id]
		e.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		var opts struct{ Detach bool }
		_ = json.NewDecoder(r.Body).Decode(&opts)
		if opts.Detach {
			e.mu.Lock()
			e.interruptions++
			for _, turn := range e.turns {
				if turn.Session == cmd.Session && !turn.Done {
					turn.Done = true
					turn.release <- "{\"type\":\"turn.failed\",\"status\":\"interrupted\"}\n"
				}
			}
			e.mu.Unlock()
			w.WriteHeader(200)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		e.mu.Lock()
		e.connections[conn] = true
		e.mu.Unlock()
		defer func() { e.mu.Lock(); delete(e.connections, conn); e.mu.Unlock() }()
		_, _ = io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		if strings.Contains(cmd.Command, "app-server") {
			return
		} // pre-turn handshake failure -> existing exec fallback
		if !strings.Contains(cmd.Command, "codex") || !strings.Contains(cmd.Command, "exec") {
			e.mu.Lock()
			e.unexpected = append(e.unexpected, "unexpected exec: "+cmd.Command)
			e.mu.Unlock()
			return
		}
		prompt, err := io.ReadAll(io.LimitReader(conn, 4<<20))
		if err != nil {
			return
		}
		turn := &invocation{ID: id, Session: cmd.Session, Prompt: string(prompt), Resume: strings.Contains(cmd.Command, "resume"), conn: conn, release: make(chan string, 1)}
		e.mu.Lock()
		e.turns = append(e.turns, turn)
		e.mu.Unlock()
		select {
		case data := <-turn.release:
			_, _ = io.WriteString(stdcopy.NewStdWriter(conn, stdcopy.Stdout), data)
		case <-time.After(3 * time.Minute):
		}
	case strings.Contains(path, "/exec/") && strings.HasSuffix(path, "/json"):
		output(w, map[string]any{"Running": false, "ExitCode": 0})
	default:
		e.mu.Lock()
		e.unexpected = append(e.unexpected, r.Method+" "+path)
		e.mu.Unlock()
		w.WriteHeader(404)
	}
}
func main() {
	mode := flag.String("mode", "", "seed or engine")
	dir := flag.String("dir", "", "disposable directory")
	listen := flag.String("listen", "127.0.0.1:0", "listener")
	token := flag.String("token", "", "test control token")
	flag.Parse()
	if *dir == "" {
		log.Fatal("directory required")
	}
	if *mode == "seed" {
		seed(*dir, *listen)
		return
	}
	if *mode != "engine" || *token == "" {
		log.Fatal("invalid fixture mode/token")
	}
	db := dbFor(*dir, false)
	must(db.Close())
	e := &engine{dir: *dir, token: *token, commands: make(map[string]struct{ Session, Command string }), connections: map[net.Conn]bool{}}
	ln, err := net.Listen("tcp", *listen)
	must(err)
	must(json.NewEncoder(os.Stdout).Encode(map[string]string{"url": "http://" + ln.Addr().String()}))
	must(http.Serve(ln, e))
}
