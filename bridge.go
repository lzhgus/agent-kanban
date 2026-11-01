package main

// The board's MCP server, through which agents work with it and with each
// other: ask you a question, file tasks for any agent, read the board. It
// speaks MCP's streamable HTTP transport, in its plain-JSON form, on the
// loopback interface; each turn gets a URL with a token of its own, which
// stops working when the turn ends.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const bridgeName = "kanban" // the MCP server's name: Claude Code calls its tools mcp__kanban__<tool>

type bridge struct {
	b    *board
	base string // http://127.0.0.1:<port>/mcp/

	mu    sync.Mutex
	turns map[string]*bridgeTurn // by token
}

// bridgeTurn is a turn the bridge serves.
type bridgeTurn struct {
	task     *Task
	reviewer bool // reviewers read the board; they do not ask or file tasks
	created  int  // tasks it filed: at most maxCreated
}

const maxCreated = 5

func newBridge(b *board) *bridge { return &bridge{b: b, turns: map[string]*bridgeTurn{}} }

// open gives a turn its URL; close takes it back.
func (br *bridge) open(t *Task, reviewer bool) (url string, close func()) {
	var key [16]byte
	rand.Read(key[:])
	token := hex.EncodeToString(key[:])
	br.mu.Lock()
	br.turns[token] = &bridgeTurn{task: t, reviewer: reviewer}
	br.mu.Unlock()
	return br.base + token, func() {
		br.mu.Lock()
		delete(br.turns, token)
		br.mu.Unlock()
	}
}

type rpcMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (br *bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	br.mu.Lock()
	turn := br.turns[strings.TrimPrefix(r.URL.Path, "/mcp/")]
	br.mu.Unlock()
	switch {
	case turn == nil:
		http.Error(w, "no such turn", http.StatusNotFound)
		return
	case r.Method == http.MethodDelete: // a client closing its session
		w.WriteHeader(http.StatusOK)
		return
	case r.Method != http.MethodPost: // no server-sent stream: every answer is in its response
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var m rpcMsg
	if err != nil || json.Unmarshal(body, &m) != nil {
		writeRPC(w, nil, nil, &rpcError{-32700, "parse error"})
		return
	}
	if len(m.ID) == 0 { // a notification, as notifications/initialized
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rerr := br.handle(turn, m)
	writeRPC(w, m.ID, result, rerr)
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, err *rpcError) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if err != nil {
		resp["error"] = err
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (br *bridge) handle(turn *bridgeTurn, m rpcMsg) (any, *rpcError) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(m.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-03-26"
		}
		return map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": bridgeName, "version": "0.1.0"},
			"instructions":    "The Agent Kanban board you are working on: ask the user, file tasks for other agents, read the board.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		var tools []map[string]any
		for _, t := range bridgeTools {
			if !turn.reviewer || t.readOnly {
				tools = append(tools, map[string]any{"name": t.name, "description": t.description, "inputSchema": t.schema})
			}
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		text, err := br.call(turn, p.Name, p.Arguments)
		if err != nil {
			return map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}, nil
		}
		return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}, nil
	}
	return nil, &rpcError{-32601, "method not found: " + m.Method}
}

// call runs a tool on the main thread, where the board lives.
func (br *bridge) call(turn *bridgeTurn, name string, args json.RawMessage) (string, error) {
	var tool *bridgeTool
	for i := range bridgeTools {
		if bridgeTools[i].name == name && (!turn.reviewer || bridgeTools[i].readOnly) {
			tool = &bridgeTools[i]
		}
	}
	if tool == nil {
		return "", fmt.Errorf("no tool %q", name)
	}
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	type answer struct {
		text string
		err  error
	}
	done := make(chan answer, 1)
	br.b.update(func() {
		text, err := tool.run(br.b, turn, args)
		done <- answer{text, err}
	})
	select {
	case a := <-done:
		return a.text, a.err
	case <-time.After(30 * time.Second):
		return "", fmt.Errorf("the board did not answer")
	}
}

type bridgeTool struct {
	name, description string
	schema            map[string]any
	readOnly          bool // reviewers get it too
	run               func(b *board, turn *bridgeTurn, args json.RawMessage) (string, error)
}

func object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

var bridgeTools = []bridgeTool{
	{
		name: "ask_user",
		description: "Ask the user a question you need answered to go on, when a real decision is theirs. " +
			"It shows on the board; end your turn right after asking, with a short summary: the answer arrives as your next message.",
		schema: object(map[string]any{
			"question": str("The question, with what the user needs to answer it."),
			"options":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 4, "description": "Up to 4 answers to choose from; the user can also write their own."},
		}, "question"),
		run: func(b *board, turn *bridgeTurn, args json.RawMessage) (string, error) {
			var a struct {
				Question string   `json:"question"`
				Options  []string `json:"options"`
			}
			if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.Question) == "" {
				return "", fmt.Errorf("ask_user needs a question")
			}
			t := turn.task
			t.Question = &Question{Text: strings.TrimSpace(a.Question), Options: a.Options[:min(len(a.Options), 4)], Turn: t.Runs}
			t.addLog("? " + t.Question.Text)
			b.save()
			return "Your question is on the board. End your turn now with a one-line summary; the user's answer will be your next message.", nil
		},
	},
	{
		name: "create_todo",
		description: "File a new task on the board, for yourself or another agent (claude, antigravity, copilot), in the same folder as this one. " +
			"Use it for follow-up work outside this task, or work another agent should do. With start, it runs right away; otherwise it waits in To Do.",
		schema: object(map[string]any{
			"title":      str("A short title."),
			"prompt":     str("What the agent should do, with the context it needs: it does not see this conversation."),
			"agent":      map[string]any{"type": "string", "enum": agentNames, "description": "Who does it; this task's agent if left out."},
			"plan_first": map[string]any{"type": "boolean", "description": "Plan, and wait for the user to confirm, before building."},
			"start":      map[string]any{"type": "boolean", "description": "Run it right away."},
		}, "title", "prompt"),
		run: func(b *board, turn *bridgeTurn, args json.RawMessage) (string, error) {
			var a struct {
				Title, Prompt, Agent string
				PlanFirst            bool `json:"plan_first"`
				Start                bool
			}
			if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.Prompt) == "" {
				return "", fmt.Errorf("create_todo needs a title and a prompt")
			}
			if turn.created >= maxCreated {
				return "", fmt.Errorf("a turn files %d tasks at most", maxCreated)
			}
			from := turn.task
			if a.Agent == "" {
				a.Agent = from.Agent
			}
			if drivers[a.Agent] == nil {
				return "", fmt.Errorf("no agent %q: one of %s", a.Agent, strings.Join(agentNames, ", "))
			}
			title := strings.TrimSpace(a.Title)
			if title == "" {
				title = firstLine(a.Prompt)
			}
			turn.created++
			b.NextID++
			t := &Task{ID: b.NextID, Title: title, Prompt: strings.TrimSpace(a.Prompt), Agent: a.Agent, Dir: from.Dir, Status: Todo, From: from.ID, PlanFirst: a.PlanFirst}
			b.Tasks = append(b.Tasks, t)
			from.addLog(fmt.Sprintf("+ filed #%d for %s: %s", t.ID, t.Agent, title))
			if a.Start {
				b.start(t)
			} else {
				b.save()
			}
			return fmt.Sprintf("Filed task #%d for %s: %s (%s).", t.ID, t.Agent, title, t.Status), nil
		},
	},
	{
		name:        "list_todos",
		description: "List the tasks on the board: id, status, agent, title and branch.",
		schema:      object(map[string]any{}),
		readOnly:    true,
		run: func(b *board, turn *bridgeTurn, args json.RawMessage) (string, error) {
			var lines []string
			for _, t := range b.Tasks {
				line := fmt.Sprintf("#%d [%s] %s: %s", t.ID, t.Status, t.Agent, t.Title)
				if t.WS != nil {
					line += " (" + t.WS.Branch + ")"
				}
				if t == turn.task {
					line += " ← this task"
				}
				lines = append(lines, line)
			}
			if len(lines) == 0 {
				return "The board is empty.", nil
			}
			return strings.Join(lines, "\n"), nil
		},
	},
	{
		name:        "get_todo",
		description: "Read a task: its prompt, status, branch and changes, current plan, and the agent's last report.",
		schema:      object(map[string]any{"id": map[string]any{"type": "integer", "description": "The task's number."}}, "id"),
		readOnly:    true,
		run: func(b *board, turn *bridgeTurn, args json.RawMessage) (string, error) {
			var a struct{ ID int }
			json.Unmarshal(args, &a)
			for _, t := range b.Tasks {
				if t.ID == a.ID {
					return describe(t), nil
				}
			}
			return "", fmt.Errorf("no task #%d: list_todos shows them", a.ID)
		},
	},
}

func describe(t *Task) string {
	var s strings.Builder
	fmt.Fprintf(&s, "#%d %s\nstatus: %s · agent: %s · folder: %s\n", t.ID, t.Title, t.Status, t.Agent, t.Dir)
	if t.WS != nil {
		fmt.Fprintf(&s, "branch: %s, forked from %s (%s); %d files changed, +%d −%d\n", t.WS.Branch, t.WS.Base, t.WS.BaseSHA, t.Changes.Files, t.Changes.Added, t.Changes.Deleted)
	}
	fmt.Fprintf(&s, "\nprompt:\n%s\n", clip(t.Prompt, 4000))
	if n := len(t.Plans); n > 0 {
		fmt.Fprintf(&s, "\nplan (v%d):\n%s\n", n, clip(t.Plans[n-1], 8000))
	}
	if t.Result != "" {
		fmt.Fprintf(&s, "\nlast report:\n%s\n", clip(t.Result, 4000))
	}
	return s.String()
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
