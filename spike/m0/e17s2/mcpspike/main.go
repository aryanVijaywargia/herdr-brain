// Command mcpspike is the E17-S2 test MCP server (stdio, JSON-RPC 2.0, one message per line).
//
// It logs only the four names HERDR_ENV, HERDR_SOCKET_PATH, HERDR_PANE_ID and
// HERDR_WORKSPACE_ID (set or not, and the value), never the whole environment.
// Without HERDR_ENV=1 it returns an empty tool list. With it, it offers one
// harmless tool, spike_echo, that returns its text and a fixed marker.
//
// Usage: mcpspike -log <file> -case <name>
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// herdrNames are the only environment names that the server reads.
var herdrNames = []string{"HERDR_ENV", "HERDR_SOCKET_PATH", "HERDR_PANE_ID", "HERDR_WORKSPACE_ID"}

// marker is a fixed spike marker that spike_echo returns, so a reply shows that the tool ran.
const marker = "mcp-echo-7316"

type envValue struct {
	Set   bool   `json:"set"`
	Value string `json:"value,omitempty"`
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type logger struct {
	f    *os.File
	name string
}

func (l *logger) write(event string, fields map[string]any) {
	rec := map[string]any{"time": time.Now().Format(time.RFC3339Nano), "case": l.name, "pid": os.Getpid(), "event": event}
	for k, v := range fields {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	fmt.Fprintln(l.f, string(b))
}

func herdrEnv() map[string]envValue {
	out := map[string]envValue{}
	for _, n := range herdrNames {
		v, ok := os.LookupEnv(n)
		out[n] = envValue{Set: ok, Value: v}
	}
	return out
}

// parentName returns the command name of the parent process, to show which CLI started the server.
func parentName() string {
	b, err := exec.Command("ps", "-o", "comm=", "-p", fmt.Sprint(os.Getppid())).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func tools(inHerdr bool) []map[string]any {
	if !inHerdr {
		return []map[string]any{}
	}
	return []map[string]any{{
		"name":        "spike_echo",
		"description": "Test tool of the E17-S2 spike. Returns the given text and a fixed marker. It has no side effects.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []string{"text"},
		},
	}}
}

func main() {
	logPath := flag.String("log", "/private/tmp/e17s2/logs/mcp.jsonl", "log file (JSON lines)")
	caseName := flag.String("case", "", "case name for the log")
	flag.Parse()

	f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcpspike:", err)
		os.Exit(1)
	}
	defer f.Close()
	log := &logger{f: f, name: *caseName}

	inHerdr := os.Getenv("HERDR_ENV") == "1"
	log.write("start", map[string]any{"ppid": os.Getppid(), "parent": parentName(), "herdr_env": herdrEnv()})

	out := json.NewEncoder(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	for in.Scan() {
		var req request
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			log.write("bad_json", map[string]any{"error": err.Error()})
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string         `json:"protocolVersion"`
				ClientInfo      map[string]any `json:"clientInfo"`
			}
			_ = json.Unmarshal(req.Params, &p)
			log.write("initialize", map[string]any{"protocol": p.ProtocolVersion, "client": p.ClientInfo})
			result = map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "mcpspike", "version": "0.1.0"},
			}
		case "tools/list":
			t := tools(inHerdr)
			log.write("tools_list", map[string]any{"count": len(t)})
			result = map[string]any{"tools": t}
		case "tools/call":
			var p struct {
				Name      string `json:"name"`
				Arguments struct {
					Text string `json:"text"`
				} `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			log.write("tools_call", map[string]any{"tool": p.Name, "text": p.Arguments.Text, "in_herdr": inHerdr})
			if !inHerdr || p.Name != "spike_echo" {
				result = map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": "refused: no such tool here"}}}
				break
			}
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "spike echo: " + p.Arguments.Text + " (" + marker + ")"}}}
		case "ping":
			result = map[string]any{}
		default:
			if len(req.ID) == 0 {
				log.write("notification", map[string]any{"method": req.Method})
				continue
			}
			log.write("unknown", map[string]any{"method": req.Method})
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
			continue
		}
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
	log.write("exit", map[string]any{"scan_error": fmt.Sprint(in.Err())})
}
