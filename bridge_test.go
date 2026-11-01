package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
)

// mcpPost sends one JSON-RPC message, as an MCP client does over HTTP.
func mcpPost(url, method string, id int, params any) (int, map[string]any, error) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if id > 0 {
		msg["id"] = id
	}
	body, _ := json.Marshal(msg)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(data, &out)
	return resp.StatusCode, out, nil
}

// callTool calls a tool while the test runs the board's main thread.
func callTool(t *testing.T, url, name string, args any) (text string, isError bool) {
	t.Helper()
	got := make(chan map[string]any, 1)
	go func() {
		_, out, err := mcpPost(url, "tools/call", 7, map[string]any{"name": name, "arguments": args})
		if err != nil {
			out = map[string]any{"error": err.Error()}
		}
		got <- out
	}()
	for {
		select {
		case f := <-queue:
			f()
		case out := <-got:
			res, _ := out["result"].(map[string]any)
			if res == nil {
				t.Fatalf("%s: %v", name, out)
			}
			content := res["content"].([]any)[0].(map[string]any)
			return content["text"].(string), res["isError"] == true
		}
	}
}

func bridgeBoard(t *testing.T) (*board, *bridge, func(*Task)) {
	b, drain := boardWith(t)
	br := newBridge(b)
	srv := httptest.NewServer(br)
	t.Cleanup(srv.Close)
	br.base = srv.URL + "/mcp/"
	b.bridge = br
	return b, br, drain
}

func TestBridgeProtocol(t *testing.T) {
	b, br, _ := bridgeBoard(t)
	task := &Task{ID: 1, Title: "API", Agent: "claude", Dir: t.TempDir(), Status: Running, Runs: 1}
	b.Tasks, b.NextID = []*Task{task}, 1
	url, closeIt := br.open(task, false)

	code, out, _ := mcpPost(url, "initialize", 1, map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
	if res := out["result"].(map[string]any); code != 200 || res["protocolVersion"] != "2025-06-18" || res["serverInfo"].(map[string]any)["name"] != "kanban" {
		t.Errorf("initialize: %d %v", code, out)
	}
	if code, _, _ := mcpPost(url, "notifications/initialized", 0, nil); code != http.StatusAccepted {
		t.Errorf("a notification: %d", code)
	}
	_, out, _ = mcpPost(url, "tools/list", 2, nil)
	if n := len(out["result"].(map[string]any)["tools"].([]any)); n != 7 {
		t.Errorf("%d tools for a builder", n)
	}
	if _, out, _ = mcpPost(url, "nope", 3, nil); out["error"] == nil {
		t.Errorf("an unknown method: %v", out)
	}
	if resp, _ := http.Get(url); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", resp.StatusCode)
	}

	// The tools, run on the board.
	if text, isErr := callTool(t, url, "create_todo", map[string]any{"title": "Docs", "prompt": "Write the docs", "agent": "antigravity"}); isErr || !strings.Contains(text, "#2 for antigravity") {
		t.Errorf("create_todo: %q", text)
	}
	if d := b.Tasks[1]; d.From != 1 || d.Dir != task.Dir || d.Status != Todo || d.Agent != "antigravity" {
		t.Errorf("the task filed: %+v", d)
	}
	if text, _ := callTool(t, url, "list_todos", nil); !strings.Contains(text, "#1 [Running] claude: API ← this task") || !strings.Contains(text, "#2 [To Do] antigravity: Docs") {
		t.Errorf("list_todos: %q", text)
	}
	if text, _ := callTool(t, url, "get_todo", map[string]any{"id": 2}); !strings.Contains(text, "Write the docs") {
		t.Errorf("get_todo: %q", text)
	}
	if text, isErr := callTool(t, url, "get_todo", map[string]any{"id": 9}); !isErr || !strings.Contains(text, "no task #9") {
		t.Errorf("get_todo of nothing: %q", text)
	}
	if _, isErr := callTool(t, url, "create_todo", map[string]any{"title": "x", "prompt": "y", "agent": "gpt"}); !isErr {
		t.Error("create_todo took an unknown agent")
	}
	if text, isErr := callTool(t, url, "ask_user", map[string]any{"question": "Which DB?", "options": []string{"Postgres", "SQLite"}}); isErr || !strings.Contains(text, "End your turn") {
		t.Errorf("ask_user: %q", text)
	}
	if q := task.Question; q == nil || q.Text != "Which DB?" || len(q.Options) != 2 || q.Turn != 1 {
		t.Errorf("the question: %+v", q)
	}
	for i := 0; i < maxCreated; i++ {
		callTool(t, url, "create_todo", map[string]any{"title": "x", "prompt": "y"})
	}
	if len(b.Tasks) != 1+maxCreated {
		t.Errorf("%d tasks: a turn files %d at most", len(b.Tasks), maxCreated)
	}

	// A reviewer reads; a closed turn's URL is gone.
	rurl, rclose := br.open(task, true)
	_, out, _ = mcpPost(rurl, "tools/list", 4, nil)
	if n := len(out["result"].(map[string]any)["tools"].([]any)); n != 3 {
		t.Errorf("%d tools for a reviewer", n)
	}
	if _, isErr := callTool(t, rurl, "ask_user", map[string]any{"question": "?"}); !isErr {
		t.Error("a reviewer asked the user")
	}
	rclose()
	closeIt()
	if code, _, _ := mcpPost(url, "tools/list", 5, nil); code != http.StatusNotFound {
		t.Errorf("a closed turn's URL answered %d", code)
	}
}

// An agent asks while planning: the task waits at Confirm with the
// question, and an option clicked answers it. It files a task for another
// agent, which starts, and plans first.
func TestBridgeAgents(t *testing.T) {
	var prompts []string
	saved := drivers
	drivers = map[string]driver{
		"claude": func(ctx context.Context, task Task, log emit) (result, error) {
			prompts = append(prompts, task.Prompt)
			if len(prompts) == 1 { // planning: it asks first
				_, out, err := mcpPost(task.mcpURL, "tools/call", 1, map[string]any{"name": "ask_user",
					"arguments": map[string]any{"question": "REST or gRPC?", "options": []string{"REST", "gRPC"}}})
				if err != nil || out["result"] == nil {
					return result{}, fmt.Errorf("ask_user: %v %v", err, out)
				}
				return result{Text: "Asked which protocol.", SessionRef: "s"}, nil
			}
			mcpPost(task.mcpURL, "tools/call", 2, map[string]any{"name": "create_todo",
				"arguments": map[string]any{"title": "Client SDK", "prompt": "Write a client for the API", "agent": "copilot", "start": true}})
			return result{Text: "## Plan\n1. REST API", SessionRef: "s"}, nil
		},
		"copilot": func(ctx context.Context, task Task, log emit) (result, error) {
			return result{Text: "SDK written"}, nil
		},
	}
	defer func() { drivers = saved }()
	b, _, drain := bridgeBoard(t)
	b.dir, b.prompt, b.title, b.agent, b.PlanFirst = t.TempDir(), "Build an API", "API", "claude", true
	b.add(true)
	task := b.Tasks[0]
	drain(task)
	if task.Status != Confirm || task.Question == nil || len(task.Plans) != 0 {
		t.Fatalf("after asking: %+v", task)
	}

	b.selected = task
	tt := ui.NewTester(b.view, 1440, 880)
	tt.Frame()
	if !tt.HasText("REST or gRPC?") || !tt.HasText("Question") || tt.HasText("Plan ready") {
		t.Fatalf("the question is not shown: %q", tt.Texts())
	}
	if _, ok := tt.Find("Confirm plan"); ok {
		t.Error("a question offered to confirm a plan there is not")
	}
	if err := tt.Click("gRPC"); err != nil {
		t.Fatal(err)
	}
	drain(task)
	if !strings.HasPrefix(prompts[1], "gRPC") || task.Question != nil || task.Status != Confirm || len(task.Plans) != 1 {
		t.Errorf("after answering: %+v, prompt %q", task, prompts[1])
	}
	if len(b.Tasks) != 2 {
		t.Fatalf("%d tasks", len(b.Tasks))
	}
	sdk := b.Tasks[1]
	drain(sdk)
	// What an agent files plans first, started or not: a person confirms it.
	if sdk.Agent != "copilot" || sdk.From != task.ID || !sdk.ByAgent || !sdk.PlanFirst || sdk.Status != Confirm || !strings.Contains(strings.Join(task.Log, "\n"), "+ filed #2 for copilot") {
		t.Errorf("the task filed: %+v", sdk)
	}
}

func TestClaudeMCPConfig(t *testing.T) {
	a := strings.Join(claudeArgs(Task{Prompt: "x", mcpURL: "http://127.0.0.1:1/mcp/tok"}), " ")
	if !strings.Contains(a, `--mcp-config={"mcpServers":{"kanban":{"type":"http","url":"http://127.0.0.1:1/mcp/tok"}}}`) || strings.Contains(a, "strict") {
		t.Errorf("args %s", a)
	}
}
