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
)

func TestToolName(t *testing.T) {
	for in, want := range map[string]string{
		"Bash": "Bash", "run_terminal_command": "Bash", "bash": "Bash", "run_command": "Bash", "read_file": "Read",
		"search_replace": "Edit", "write": "Write", "search_tool": "ToolSearch",
		"mcp__kanban__ask_user": "kanban/ask_user", "kanban__ask_user": "kanban/ask_user", "kanban_ask_user": "kanban/ask_user",
		"kanban-ask_user": "kanban/ask_user", "mcp__agent-kanban__create_todo": "agent-kanban/create_todo",
		"mcp__github__create_issue": "github/create_issue", "kanbanize": "kanbanize", "codemode": "codemode",
	} {
		if got := toolName(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

func TestKimiConfig(t *testing.T) {
	in := "default_model = \"kimi-code/k3\"\n[thinking]\nenabled = true\n[services.moonshot_search]\napi_key = \"k\"\n" +
		"[providers.\"managed:kimi-code\"]\ntype = \"kimi\"\n[models.\"kimi-code/k3\"]\nprovider = \"p\"\n" +
		"[[hooks]]\ncommand = \"notify\"\n[mcp]\nservers = 1\n[models.\"kimi-code/k3.1\"]\nprovider = \"p\"\n"
	want := "default_model = \"kimi-code/k3\"\n[thinking]\nenabled = true\n[services.moonshot_search]\napi_key = \"k\"\n" +
		"[providers.\"managed:kimi-code\"]\ntype = \"kimi\"\n[models.\"kimi-code/k3\"]\nprovider = \"p\"\n" +
		"[models.\"kimi-code/k3.1\"]\nprovider = \"p\"\n"
	if got := kimiConfig(in); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// fakeACP plays Kimi Code or OpenCode over ACP: each announces a tool call
// before its input, as they do; Kimi asks a permission and keeps its token
// counts in wire.jsonl, OpenCode puts them in the prompt's reply.
func fakeACP(name string) {
	switch name {
	case "kimi":
		config, _ := os.ReadFile(filepath.Join(os.Getenv("KIMI_CODE_HOME"), "config.toml"))
		fakeLog("args %s config=%q", strings.Join(os.Args[1:], " "), config)
	case "opencode":
		config, _ := os.ReadFile(filepath.Join(os.Getenv("OPENCODE_CONFIG_DIR"), "opencode.json"))
		var c map[string]any
		json.Unmarshal(config, &c)
		mcp, _ := json.Marshal(c["mcp"])
		fakeLog("args %s mcp=%s data=%v", strings.Join(os.Args[1:], " "), mcp, os.Getenv("XDG_DATA_HOME") != "")
	}
	in := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	reply := func(id json.RawMessage, result any) {
		out.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	update := func(u map[string]any) {
		out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s-1", "update": u}})
	}
	type msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			SessionID  string `json:"sessionId"`
			Cwd        string `json:"cwd"`
			ConfigID   string `json:"configId"`
			Value      string `json:"value"`
			MCPServers []struct {
				Name, URL string
			} `json:"mcpServers"`
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
	cwd := ""
	for {
		m, ok := next()
		if !ok {
			return
		}
		switch m.Method {
		case "initialize":
			reply(m.ID, map[string]any{"protocolVersion": 1})
		case "session/new":
			cwd = m.Params.Cwd
			fakeLog("new mcp=%s=%s", m.Params.MCPServers[0].Name, m.Params.MCPServers[0].URL)
			reply(m.ID, map[string]string{"sessionId": "s-1"})
		case "session/load":
			cwd = m.Params.Cwd
			fakeLog("load %s", m.Params.SessionID)
			reply(m.ID, map[string]any{})
		case "session/set_config_option":
			fakeLog("set %s=%s", m.Params.ConfigID, m.Params.Value)
			reply(m.ID, map[string]any{})
		case "session/prompt":
			if name == "kimi" {
				update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c1", "title": "Bash", "status": "pending"})
				update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "status": "in_progress"})
				out.Encode(map[string]any{"jsonrpc": "2.0", "id": 0, "method": "session/request_permission", "params": map[string]any{"options": []map[string]string{
					{"optionId": "approve_once", "kind": "allow_once"}, {"optionId": "approve_always", "kind": "allow_always"}, {"optionId": "reject", "kind": "reject_once"}}}})
				answer, _ := next()
				fakeLog("permission %s", answer.Result)
				update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "title": "Running: ls", "status": "in_progress", "rawInput": map[string]string{"command": "ls"}})
				wire := filepath.Join(os.Getenv("KIMI_CODE_HOME"), "sessions", "wd_x", "s-1", "agents", "main")
				os.MkdirAll(wire, 0o700)
				f, _ := os.OpenFile(filepath.Join(wire, "wire.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				json.NewEncoder(f).Encode(map[string]any{"type": "context.append_loop_event", "event": map[string]any{"type": "step.end",
					"usage": map[string]int{"inputOther": 5, "output": 2, "inputCacheRead": 7}}})
				f.Close()
				update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "re: " + m.Params.Prompt[0].Text}})
				reply(m.ID, map[string]string{"stopReason": "end_turn"})
				continue
			}
			update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c1", "title": "bash", "status": "pending", "rawInput": map[string]string{"cwd": cwd}})
			update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "title": "ls", "status": "completed"})
			update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c2", "title": "kanban_ask_user", "status": "in_progress", "rawInput": map[string]string{"question": "Red?"}})
			update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c3", "title": "write", "status": "pending", "rawInput": map[string]string{}})
			real, _ := filepath.EvalSymlinks(cwd) // as OpenCode titles it: the real path, no leading slash
			update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c3", "title": strings.TrimPrefix(real, "/") + "/hi.txt", "status": "completed"})
			update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c4", "title": "read", "status": "pending"}) // never finished
			update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "re: " + m.Params.Prompt[0].Text}})
			reply(m.ID, map[string]any{"stopReason": "end_turn", "usage": map[string]int{"inputTokens": 3, "outputTokens": 1, "cachedReadTokens": 9}})
		default:
			reply(m.ID, map[string]any{})
		}
	}
}

func TestFakeKimi(t *testing.T) {
	calls := fakeCLIs(t)
	user := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", user)
	os.WriteFile(filepath.Join(user, "config.toml"), []byte("default_model = \"k3\"\n[[hooks]]\ncommand = \"x\"\n"), 0o600)
	os.MkdirAll(filepath.Join(user, "credentials"), 0o700)
	lines, log := collect()
	dir := t.TempDir()
	r, err := runKimi(context.Background(), Task{Dir: dir, Prompt: "hello", Model: "kimi-code/k3", mcpURL: "http://127.0.0.1:1/mcp/x"}, log)
	if err != nil {
		t.Fatal(err)
	}
	if r.SessionRef != "s-1" || r.Text != "re: hello" || r.Usage != (Usage{Input: 5, Output: 2, CacheRead: 7}) {
		t.Errorf("%+v", r)
	}
	want := []string{`args acp config="default_model = \"k3\""`, "new mcp=kanban=http://127.0.0.1:1/mcp/x", "set mode=auto", "set model=kimi-code/k3",
		`permission {"outcome":{"optionId":"approve_always","outcome":"selected"}}`}
	if got := readLog(t, calls); !slices.Equal(got, want) {
		t.Errorf("calls\n%q\nwant\n%q", got, want)
	}
	if got := strings.Join(*lines, "\n"); got != "▸ Bash ls\nre: hello" {
		t.Errorf("log %q", got)
	}
	home := filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "agent-kanban", "kimi-home")
	if link, _ := os.Readlink(filepath.Join(home, "credentials")); link != filepath.Join(user, "credentials") {
		t.Errorf("credentials: %q", link)
	}
	// A reply resumes the session, and counts only its own steps.
	r, err = runKimi(context.Background(), Task{Dir: dir, Prompt: "more", SessionRef: "s-1", resume: true}, func(string) {})
	if err != nil || r.Usage.Input != 5 || !slices.Contains(readLog(t, calls), "load s-1") {
		t.Errorf("reply: %+v, %v", r, err)
	}
}

func TestFakeOpencode(t *testing.T) {
	calls := fakeCLIs(t)
	config, data := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("XDG_DATA_HOME", data)
	os.MkdirAll(filepath.Join(config, "opencode"), 0o700)
	os.WriteFile(filepath.Join(config, "opencode", "opencode.json"), []byte(`{"mcp":{"notes":{"type":"local","command":["notes"]}}}`), 0o600)
	os.MkdirAll(filepath.Join(data, "opencode"), 0o700)
	os.WriteFile(filepath.Join(data, "opencode", "auth.json"), []byte("{}"), 0o600)
	lines, log := collect()
	dir := t.TempDir()
	r, err := runOpencode(context.Background(), Task{Dir: dir, Prompt: "hello", mcpURL: "http://127.0.0.1:1/mcp/x"}, log)
	if err != nil {
		t.Fatal(err)
	}
	if r.SessionRef != "s-1" || r.Text != "re: hello" || r.Usage != (Usage{Input: 3, Output: 1, CacheRead: 9}) {
		t.Errorf("%+v", r)
	}
	want := []string{`args acp --pure mcp={"notes":{"enabled":false}} data=true`, "new mcp=kanban=http://127.0.0.1:1/mcp/x"}
	if got := readLog(t, calls); !slices.Equal(got, want) {
		t.Errorf("calls\n%q\nwant\n%q", got, want)
	}
	if got := strings.Join(*lines, "\n"); got != "▸ Bash ls\n▸ kanban/ask_user\n▸ Write hi.txt\nre: hello\n▸ Read" {
		t.Errorf("log %q", got)
	}
	home := filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "agent-kanban", "opencode-home")
	if link, _ := os.Readlink(filepath.Join(home, "data", "opencode", "auth.json")); link != filepath.Join(data, "opencode", "auth.json") {
		t.Errorf("auth.json: %q", link)
	}
}
