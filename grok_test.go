package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeGrok serves `grok agent stdio`: sessions g-new on session/new, any on
// session/load but "gone" (it replays the old conversation first), and one
// reply per prompt. A prompt "slow…" waits for an interject and answers
// both; "ask" puts up a permission first.
func fakeGrok() {
	fakeLog("args %s home=%v claude-mcps=%s", strings.Join(os.Args[1:], " "), os.Getenv("GROK_HOME") != "", os.Getenv("GROK_CLAUDE_MCPS_ENABLED"))
	in := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	reply := func(id json.RawMessage, result any) {
		out.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	update := func(method string, u map[string]any) {
		out.Encode(map[string]any{"jsonrpc": "2.0", "method": method, "params": map[string]any{"sessionId": "x", "update": u}})
	}
	type msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			SessionID  string `json:"sessionId"`
			Text       string `json:"text"`
			MCPServers []struct {
				Name, URL string
			} `json:"mcpServers"`
			Meta struct {
				Yolo bool `json:"yoloMode"`
			} `json:"_meta"`
			Prompt []struct{ Text string } `json:"prompt"`
		} `json:"params"`
		Result json.RawMessage `json:"result"`
	}
	next := func() (m msg, ok bool) {
		if !in.Scan() {
			return m, false
		}
		json.Unmarshal(in.Bytes(), &m)
		return m, true
	}
	for {
		m, ok := next()
		if !ok {
			return
		}
		switch m.Method {
		case "initialize":
			reply(m.ID, map[string]any{"protocolVersion": 1})
		case "session/new", "session/load":
			var servers []string
			for _, s := range m.Params.MCPServers {
				servers = append(servers, s.Name+"="+s.URL)
			}
			fakeLog("%s %s yolo=%v mcp=%v", m.Method, m.Params.SessionID, m.Params.Meta.Yolo, servers)
			if m.Params.SessionID == "gone" {
				out.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32002, "message": "session not found"}})
				continue
			}
			if m.Method == "session/load" {
				update("session/update", map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "an old answer"}})
				reply(m.ID, map[string]any{})
				continue
			}
			reply(m.ID, map[string]string{"sessionId": "g-new"})
		case "session/prompt":
			text := m.Params.Prompt[0].Text
			fakeLog("prompt %s %s", m.Params.SessionID, text)
			update("session/update", map[string]any{"sessionUpdate": "tool_call", "title": "Run", "rawInput": map[string]string{"command": "ls"},
				"_meta": map[string]any{"x.ai/tool": map[string]string{"name": "run_terminal_command"}}})
			if text == "ask" {
				out.Encode(map[string]any{"jsonrpc": "2.0", "id": "perm-1", "method": "session/request_permission", "params": map[string]any{"options": []map[string]string{
					{"optionId": "yes", "kind": "allow_once"}, {"optionId": "no", "kind": "reject_once"}}}})
				answer, _ := next()
				fakeLog("permission %s", answer.Result)
			}
			if strings.HasPrefix(text, "slow") {
				i, _ := next()
				fakeLog("%s %s", i.Method, i.Params.Text)
				reply(i.ID, map[string]any{})
				text += " + " + i.Params.Text
			}
			update("session/update", map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "re: "}})
			update("session/update", map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": text}})
			update("_x.ai/session_notification", map[string]any{"sessionUpdate": "turn_completed", "usage": map[string]float64{
				"inputTokens": 12, "cachedReadTokens": 2, "outputTokens": 3, "costUsdTicks": 1e8}})
			reply(m.ID, map[string]string{"stopReason": "end_turn"})
		case "_x.ai/auth/check_subscription":
			reply(m.ID, map[string]any{"authenticated": true, "meta": map[string]string{"subscription_tier": "SuperGrok"}})
		case "_x.ai/billing":
			reply(m.ID, map[string]any{"config": map[string]any{"creditUsagePercent": 8.0, "currentPeriod": map[string]string{
				"type": "USAGE_PERIOD_TYPE_WEEKLY", "end": "2030-01-08T06:04:30.843+00:00"}}, "subscription_tier": "SuperGrok"})
		default:
			reply(m.ID, map[string]any{})
		}
	}
}

// signInGrok gives the fake a sign-in to share, in its own Grok home.
func signInGrok(t *testing.T) string {
	user := t.TempDir()
	os.WriteFile(filepath.Join(user, "auth.json"), []byte("{}"), 0o600)
	t.Setenv("GROK_HOME", user)
	return user
}

func TestFakeGrok(t *testing.T) {
	calls := fakeCLIs(t)
	user := signInGrok(t)
	dir := t.TempDir()
	run := func(task Task) (result, []string, string) {
		t.Helper()
		lines, log := collect()
		task.Dir = dir
		r, err := runGrok(context.Background(), task, log)
		if err != nil {
			t.Fatalf("%+v: %v", task, err)
		}
		return r, readLog(t, calls), strings.Join(*lines, "\n")
	}
	r, got, lines := run(Task{Prompt: "hello", mcpURL: "http://127.0.0.1:1/mcp/x"})
	if r.SessionRef != "g-new" || r.Text != "re: hello" || r.Usage != (Usage{Input: 10, Output: 3, CacheRead: 2, CostUSD: 0.01}) {
		t.Errorf("first turn: %+v", r)
	}
	if !slices.Contains(got, "session/new  yolo=true mcp=[kanban=http://127.0.0.1:1/mcp/x]") || !strings.Contains(got[0], "agent --no-leader --always-approve stdio home=true claude-mcps=0") {
		t.Errorf("calls %q", got)
	}
	if lines != "▸ Bash ls\nre: hello" {
		t.Errorf("log %q", lines)
	}
	// Its home: the user's sign-in, linked, and the folder trusted.
	home := filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "agent-kanban", "grok-home")
	if link, err := os.Readlink(filepath.Join(home, "auth.json")); err != nil || link != filepath.Join(user, "auth.json") {
		t.Errorf("auth.json: %q, %v", link, err)
	}
	if trusted, _ := os.ReadFile(filepath.Join(home, "trusted_folders.toml")); !strings.Contains(string(trusted), "[folders.\""+dir+"\"]") {
		t.Errorf("trusted folders: %s", trusted)
	}

	r, got, lines = run(Task{Prompt: "more", SessionRef: "g-old", resume: true})
	if r.SessionRef != "g-old" || r.Text != "re: more" || !slices.Contains(got, "prompt g-old more") || strings.Contains(lines, "an old answer") {
		t.Errorf("reply: %+v, calls %q, log %q", r, got, lines)
	}
	r, got, lines = run(Task{Prompt: "again", SessionRef: "gone", resume: true}) // lost: a new session
	if r.SessionRef != "g-new" || !slices.Contains(got, "prompt g-new again") || !strings.Contains(lines, "couldn't resume") {
		t.Errorf("a lost session: %+v, calls %q, log %q", r, got, lines)
	}
	_, got, _ = run(Task{Prompt: "ask"}) // nobody is here to allow it
	if !slices.Contains(got, `permission {"outcome":{"optionId":"no","outcome":"selected"}}`) {
		t.Errorf("permission: %q", got)
	}

	// A message while it works joins the turn.
	inbox := make(chan string, 1)
	inbox <- "use tabs"
	r, got, _ = run(Task{Prompt: "slow work", inbox: inbox})
	if r.Text != "re: slow work + use tabs" || len(r.Unsent) != 0 || !slices.Contains(got, "_x.ai/interject use tabs") {
		t.Errorf("live: %+v, calls %q", r, got)
	}
}

func TestGrokNotSignedIn(t *testing.T) {
	fakeCLIs(t)
	t.Setenv("GROK_HOME", t.TempDir())
	t.Setenv("XAI_API_KEY", "")
	if _, err := runGrok(context.Background(), Task{Dir: t.TempDir(), Prompt: "hi"}, func(string) {}); err == nil || !strings.Contains(err.Error(), "grok login") {
		t.Errorf("err %v", err)
	}
}

func TestProbeGrok(t *testing.T) {
	fakeCLIs(t)
	signInGrok(t)
	q, err := probeGrok(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reset := time.Date(2030, 1, 8, 6, 4, 30, 843e6, time.UTC)
	if q.Plan != "SuperGrok" || len(q.Windows) != 1 || q.Windows[0].Label != "Week" || q.Windows[0].Used != 8 || !q.Windows[0].ResetsAt.Equal(reset) {
		t.Errorf("%+v", q)
	}
	q, err = parseGrokBilling(json.RawMessage(`{"authenticated":true}`), json.RawMessage(`{"config":{"creditUsagePercent":120,"onDemandCap":{"val":10},"onDemandUsed":{"val":2.5}}}`))
	if err != nil || len(q.Windows) != 2 || q.Windows[0].Used != 100 || q.Windows[1] != (Window{Label: "On demand", Used: 25}) {
		t.Errorf("%+v, %v", q, err)
	}
	if _, err := parseGrokBilling(json.RawMessage(`{"authenticated":false}`), nil); err == nil {
		t.Error("signed out")
	}
}
