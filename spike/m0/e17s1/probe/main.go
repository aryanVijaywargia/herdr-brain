// Command e17s1probe is the E17-S1 spike reader. It subscribes to the agent events of some
// panes in one herdr session, calls agent.get at once for each status event, and tests three
// event-key methods for the "agent stopped working" trigger (HLD §10.4):
//
//   - K: the §10.4 key alone. Each event with a stop status (idle or done) builds the key
//     (pane, agent session id, state_change_seq), both from agent.get, and claims it.
//   - KT: K, but only for a stop transition: the last status that the reader saw for the
//     pane (from the stream, or the baseline) is working.
//   - KJ: KT, and the event must join to the agent.get answer: the agent.get status is the
//     event status, and agent.get has an agent session id. Else the event is skipped.
//
// The claims of each method go to a JSON-lines file, so they live across a restart of the
// reader, as rows in automation_runs would. At start the reader takes the current state of
// each pane as a baseline and claims nothing (§10.4 step 1).
//
// A line "note <text>" in the control FIFO goes to the log. A line "dup <pane>" hands the last stop event of the pane to the
// methods again, with a new agent.get, as a duplicate delivery would come. A line
// "inject <pane> <workspace> <agent> <status>" hands a made event of that shape (the same
// bytes as a herdr event) to the methods, for a duplicate after a restart.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

type event struct {
	Event string `json:"event"`
	Data  struct {
		PaneID      string `json:"pane_id"`
		WorkspaceID string `json:"workspace_id"`
		Agent       string `json:"agent"`
		AgentStatus string `json:"agent_status"`
		Released    bool   `json:"released"`
	} `json:"data"`
}

type agentInfo struct {
	Found   bool   `json:"found"`
	Error   string `json:"error,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Status  string `json:"status,omitempty"`
	Seq     *int64 `json:"seq,omitempty"`
	Session string `json:"session,omitempty"`
	Focused bool   `json:"focused"`
	Rev     int64  `json:"revision"`
}

type record struct {
	T      string          `json:"t"`
	Run    string          `json:"run"`
	Kind   string          `json:"kind"` // baseline, event, dup, claim, skip
	Pane   string          `json:"pane,omitempty"`
	Raw    json.RawMessage `json:"raw,omitempty"`
	Prev   string          `json:"prev,omitempty"`
	Get    *agentInfo      `json:"get,omitempty"`
	GetMs  float64         `json:"get_ms,omitempty"`
	Method string          `json:"method,omitempty"`
	Key    string          `json:"key,omitempty"`
	Why    string          `json:"why,omitempty"`
}

type probe struct {
	sock   string
	run    string
	mu     sync.Mutex
	out    *os.File
	claims *os.File
	held   map[string]bool            // method + key
	prev   map[string]string          // pane → last status seen
	last   map[string]json.RawMessage // pane → last stop event
}

func main() {
	sock := flag.String("sock", "", "herdr socket of the test session")
	logPath := flag.String("log", "", "JSON-lines log")
	claimsPath := flag.String("claims", "", "JSON-lines claims (kept across restarts)")
	ctl := flag.String("ctl", "", "control FIFO")
	panes := flag.String("panes", "", "comma-separated pane ids")
	run := flag.String("run", "r1", "run label for the log")
	flag.Parse()
	if *sock == "" || *logPath == "" || *claimsPath == "" || *panes == "" {
		fmt.Fprintln(os.Stderr, "usage: e17s1probe -sock S -log L -claims C -panes w1:p1[,..] [-ctl F] [-run R]")
		os.Exit(2)
	}
	p := &probe{sock: *sock, run: *run, held: map[string]bool{}, prev: map[string]string{}, last: map[string]json.RawMessage{}}
	var err error
	if p.out, err = os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
		fail(err)
	}
	if err := p.loadClaims(*claimsPath); err != nil {
		fail(err)
	}
	ids := strings.Split(*panes, ",")
	for _, id := range ids {
		g, ms := p.get(id)
		p.prev[id] = g.Status
		p.write(record{Kind: "baseline", Pane: id, Get: &g, GetMs: ms})
	}
	if *ctl != "" {
		go p.control(*ctl)
	}
	if err := p.subscribe(ids); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "e17s1probe:", err)
	os.Exit(1)
}

func (p *probe) loadClaims(path string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		var r record
		if json.Unmarshal([]byte(line), &r) == nil && r.Kind == "claim" {
			p.held[r.Method+" "+r.Key] = true
		}
	}
	p.claims, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	return err
}

func (p *probe) write(r record) {
	r.T = time.Now().Format("15:04:05.000")
	r.Run = p.run
	b, _ := json.Marshal(r)
	p.out.Write(append(b, '\n'))
	if r.Kind == "claim" {
		p.claims.Write(append(b, '\n'))
	}
}

// call sends one request on a new connection and returns the result object.
func (p *probe) call(method string, params any) (map[string]any, error) {
	c, err := net.DialTimeout("unix", p.sock, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	req, _ := json.Marshal(map[string]any{"id": "e17s1", "method": method, "params": params})
	if _, err := c.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result map[string]any `json:"result"`
		Error  any            `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		b, _ := json.Marshal(resp.Error)
		return nil, fmt.Errorf("%s", b)
	}
	return resp.Result, nil
}

func (p *probe) get(pane string) (agentInfo, float64) {
	start := time.Now()
	res, err := p.call("agent.get", map[string]any{"target": pane})
	ms := float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		return agentInfo{Error: err.Error()}, ms
	}
	a, _ := res["agent"].(map[string]any)
	if a == nil {
		return agentInfo{Error: "no agent in result"}, ms
	}
	g := agentInfo{Found: true}
	g.Agent, _ = a["agent"].(string)
	g.Status, _ = a["agent_status"].(string)
	g.Focused, _ = a["focused"].(bool)
	if r, ok := a["revision"].(float64); ok {
		g.Rev = int64(r)
	}
	if s, ok := a["state_change_seq"].(float64); ok {
		n := int64(s)
		g.Seq = &n
	}
	if s, ok := a["agent_session"].(map[string]any); ok {
		g.Session, _ = s["value"].(string)
	}
	return g, ms
}

func (p *probe) subscribe(ids []string) error {
	subs := []map[string]any{{"type": "pane.agent_detected"}, {"type": "pane.exited"}, {"type": "pane.closed"}}
	for _, id := range ids {
		subs = append(subs, map[string]any{"type": "pane.agent_status_changed", "pane_id": id})
	}
	c, err := net.Dial("unix", p.sock)
	if err != nil {
		return err
	}
	req, _ := json.Marshal(map[string]any{"id": "e17s1sub", "method": "events.subscribe", "params": map[string]any{"subscriptions": subs}})
	if _, err := c.Write(append(req, '\n')); err != nil {
		return err
	}
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			p.write(record{Kind: "eof", Why: err.Error()})
			return err
		}
		p.handle("event", json.RawMessage(strings.TrimSpace(string(line))))
	}
}

func (p *probe) control(path string) {
	for {
		f, err := os.Open(path) // blocks until a writer opens the FIFO
		if err != nil {
			p.write(record{Kind: "ctl", Why: err.Error()})
			return
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fs := strings.Fields(sc.Text())
			if len(fs) > 1 && fs[0] == "note" {
				p.write(record{Kind: "note", Why: strings.Join(fs[1:], " ")})
				continue
			}
			if len(fs) == 5 && fs[0] == "inject" { // inject <pane> <workspace> <agent> <status>
				raw, _ := json.Marshal(map[string]any{"event": "pane.agent_status_changed",
					"data": map[string]string{"pane_id": fs[1], "workspace_id": fs[2], "agent": fs[3], "agent_status": fs[4]}})
				p.handle("dup", raw)
				continue
			}
			if len(fs) == 2 && fs[0] == "dup" {
				p.mu.Lock()
				raw := p.last[fs[1]]
				p.mu.Unlock()
				if raw == nil {
					p.write(record{Kind: "ctl", Pane: fs[1], Why: "no stop event to duplicate"})
					continue
				}
				p.handle("dup", raw)
			}
		}
		f.Close()
	}
}

func stop(s string) bool { return s == "idle" || s == "done" }

// handle gives one event (or a duplicate) to the three methods.
func (p *probe) handle(kind string, raw json.RawMessage) {
	var ev event
	if json.Unmarshal(raw, &ev) != nil || ev.Event != "pane.agent_status_changed" {
		p.write(record{Kind: kind, Raw: raw})
		return
	}
	pane, st := ev.Data.PaneID, ev.Data.AgentStatus
	g, ms := p.get(pane) // at once, before anything else
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.prev[pane]
	p.prev[pane] = st
	p.write(record{Kind: kind, Pane: pane, Raw: raw, Prev: prev, Get: &g, GetMs: ms})
	if !stop(st) {
		return
	}
	p.last[pane] = raw
	seq := "none"
	if g.Seq != nil {
		seq = fmt.Sprint(*g.Seq)
	}
	key := pane + "|" + g.Session + "|" + seq
	p.try("K", pane, key)
	if prev != "working" {
		p.skip("KT", pane, key, "previous status "+prev)
		p.skip("KJ", pane, key, "previous status "+prev)
		return
	}
	p.try("KT", pane, key)
	switch {
	case !g.Found:
		p.skip("KJ", pane, key, "agent.get: "+g.Error)
	case g.Status != st:
		p.skip("KJ", pane, key, "agent.get status "+g.Status+" is not the event status "+st)
	case g.Session == "":
		p.skip("KJ", pane, key, "agent.get has no agent session id")
	default:
		p.try("KJ", pane, key)
	}
}

func (p *probe) try(method, pane, key string) {
	if p.held[method+" "+key] {
		p.skip(method, pane, key, "key already claimed")
		return
	}
	p.held[method+" "+key] = true
	p.write(record{Kind: "claim", Method: method, Pane: pane, Key: key})
}

func (p *probe) skip(method, pane, key, why string) {
	p.write(record{Kind: "skip", Method: method, Pane: pane, Key: key, Why: why})
}
