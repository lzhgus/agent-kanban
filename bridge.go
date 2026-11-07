package main

// The board's MCP server, through which agents work with it and with each
// other: ask you a question, file tasks for any agent, read the board. It
// speaks MCP's streamable HTTP transport, in its plain-JSON form, on the
// loopback interface; each turn gets a URL with a token of its own, which
// stops working when the turn ends.

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const bridgeName = "kanban" // the MCP server's name: Claude Code calls its tools mcp__kanban__<tool>

// The board's MCP server for agents outside it, such as Claude Code in your
// terminal, is the same server at a fixed port, under a lasting token, and
// by this name; the board's own turns are kept off it.
const (
	outsideName = "agent-kanban"
	outsidePort = 47821
)

type bridge struct {
	b    *board
	base string // http://127.0.0.1:<port>/mcp/

	mu    sync.Mutex
	turns map[string]*bridgeTurn // by token
}

// bridgeTurn is a turn the bridge serves, or the agents outside the board.
type bridgeTurn struct {
	task     *Task // nil outside the board
	reviewer bool  // reviewers read the board; they do not ask or file tasks
	created  int   // tasks it filed: at most maxCreated
}

// outside reports whether it serves an agent outside the board: you, by
// way of your own agent, so its tasks are yours.
func (turn *bridgeTurn) outside() bool { return turn.task == nil }

// can reports whether the turn has the tool.
func (turn *bridgeTurn) can(t *bridgeTool) bool {
	switch {
	case turn.outside():
		return t.outside
	case turn.reviewer:
		return t.readOnly
	}
	return true
}

// openOutside serves the agents outside the board at token, for good.
func (br *bridge) openOutside(token string) string {
	br.mu.Lock()
	br.turns[token] = &bridgeTurn{}
	br.mu.Unlock()
	return br.base + token
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
			"instructions":    "The Agent Kanban board you are working on: ask the user, hand parts of your task to other agents and wait for them, message tasks, read the board.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		var tools []map[string]any
		for _, t := range bridgeTools {
			if turn.can(&t) {
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
		if bridgeTools[i].name == name && turn.can(&bridgeTools[i]) {
			tool = &bridgeTools[i]
		}
	}
	if tool == nil {
		return "", fmt.Errorf("no tool %q", name)
	}
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	if tool.blocking { // it waits here, and reads the board through sync
		return tool.run(br.b, turn, args)
	}
	var text string
	var err error
	if !br.b.sync(func() { text, err = tool.run(br.b, turn, args) }) {
		return "", fmt.Errorf("the board did not answer")
	}
	return text, err
}

// sync runs f on the main thread, where the board lives, and waits for it:
// false if the board did not get to it in time.
func (b *board) sync(f func()) bool {
	done := make(chan struct{})
	b.update(func() { f(); close(done) })
	select {
	case <-done:
		return true
	case <-time.After(30 * time.Second):
		return false
	}
}

func (b *board) task(id int) *Task {
	for _, t := range b.Tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

type bridgeTool struct {
	name, description string
	schema            map[string]any
	readOnly          bool // reviewers get it too
	outside           bool // agents outside the board get it too
	blocking          bool // run off the main thread: it waits, reading the board with b.sync
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
		description: "File a new task on the board, for yourself or another agent (" + strings.Join(agentNames, ", ") + "). " +
			"With subtask, it is part of this task, handed to another agent: it starts right away, on a new branch from this task's last commit, " +
			"needs no plan confirmed, and you can wait_todo for it, message_todo it, and git merge its branch into yours. " +
			"Without subtask, it is separate work in this task's folder, such as a follow-up: it plans first, and the user confirms its plan before it changes anything; " +
			"with start it runs right away, otherwise it waits in To Do. With read_only, its agent changes no files: research or a review, ending in a report. " +
			"Called from outside the board, as from a terminal, give the folder: the task is the user's, and plans first only with plan_first.",
		schema: object(map[string]any{
			"title":      str("A short title."),
			"prompt":     str("What the agent should do, with the context it needs: it does not see this conversation."),
			"agent":      map[string]any{"type": "string", "enum": agentChoices, "description": "Who does it: an agent, or any, whichever is free when it starts; this task's agent if left out."},
			"subtask":    map[string]any{"type": "boolean", "description": "Part of this task: it starts now, on a branch of yours, with no plan to confirm."},
			"read_only":  map[string]any{"type": "boolean", "description": "Its agent reads but changes no files, and reports what it found."},
			"start":      map[string]any{"type": "boolean", "description": "Run it right away (a subtask always does)."},
			"model":      str("The model, for the agent's CLI; its default if left out (a subtask of the same agent: this task's)."),
			"folder":     str("Outside the board only: the absolute path of the folder to work in."),
			"plan_first": map[string]any{"type": "boolean", "description": "Outside the board only: plan first, and wait for the user to confirm the plan."},
		}, "title", "prompt"),
		outside: true,
		run: func(b *board, turn *bridgeTurn, args json.RawMessage) (string, error) {
			var a struct {
				Title, Prompt, Agent, Folder, Model string
				Start, Subtask                      bool
				ReadOnly                            bool `json:"read_only"`
				PlanFirst                           bool `json:"plan_first"`
			}
			if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.Prompt) == "" {
				return "", fmt.Errorf("create_todo needs a title and a prompt")
			}
			if turn.outside() {
				return fileForUser(b, a.Title, a.Prompt, a.Agent, a.Model, a.Folder, a.Start, a.ReadOnly, a.PlanFirst)
			}
			if turn.created >= maxCreated {
				return "", fmt.Errorf("a turn files %d tasks at most", maxCreated)
			}
			from := turn.task
			if a.Agent == "" {
				a.Agent = from.Agent
			}
			if drivers[a.Agent] == nil && a.Agent != anyAgent {
				return "", fmt.Errorf("no agent %q: one of %s", a.Agent, strings.Join(agentChoices, ", "))
			}
			title := strings.TrimSpace(a.Title)
			if title == "" {
				title = firstLine(a.Prompt)
			}
			turn.created++
			b.NextID++
			t := &Task{ID: b.NextID, Title: title, Prompt: strings.TrimSpace(a.Prompt), Agent: a.Agent, Dir: from.Dir, Status: Todo, From: from.ID, ReadOnly: a.ReadOnly,
				PlanFirst: !a.Subtask, ByAgent: !a.Subtask, Sub: a.Subtask} // a person confirms what agents file, but for parts of work already theirs
			if t.Agent == from.Agent { // the same agent: the model the user chose for it
				t.Model = from.Model
			}
			t.Model = cmp.Or(strings.TrimSpace(a.Model), t.Model)
			kind := "filed"
			if a.Subtask {
				kind = "subtask"
				if from.WS != nil { // a branch of this task's branch: its merge goes into this one
					t.Dir = from.WS.Dir()
				}
			}
			b.Tasks = append(b.Tasks, t)
			from.addLog(fmt.Sprintf("+ %s #%d for %s: %s", kind, t.ID, t.Agent, title))
			if a.Start || a.Subtask {
				b.start(t)
			} else {
				b.save()
				b.autopick()
			}
			return fmt.Sprintf("Filed task #%d for %s: %s (%s).", t.ID, t.Agent, title, t.Status), nil
		},
	},
	{
		name:        "list_todos",
		description: "List the tasks on the board: id, status, agent, title and branch.",
		schema:      object(map[string]any{}),
		readOnly:    true,
		outside:     true,
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
		outside:     true,
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
	{
		name: "wait_todo",
		description: "Wait for a task to finish its turn, such as a subtask you filed, and read it: its status, branch, changes and report. " +
			"It returns when the task stops running, or after timeout_seconds with it still running: call it again to wait on.",
		schema: object(map[string]any{
			"id":              map[string]any{"type": "integer", "description": "The task's number."},
			"timeout_seconds": map[string]any{"type": "integer", "description": "How long to wait at most: 300 if left out, at most 600."},
		}, "id"),
		readOnly: true,
		blocking: true,
		outside:  true,
		run: func(b *board, turn *bridgeTurn, args json.RawMessage) (string, error) {
			var a struct {
				ID      int
				Timeout int `json:"timeout_seconds"`
			}
			json.Unmarshal(args, &a)
			if a.Timeout <= 0 {
				a.Timeout = 300
			}
			a.Timeout = min(a.Timeout, 600)
			deadline := time.Now().Add(time.Duration(a.Timeout) * time.Second)
			for logged := false; ; time.Sleep(waitPoll) {
				var desc string
				var status Status
				var found, running, gone bool
				if !b.sync(func() {
					if _, ok := b.runs[turn.taskID()]; !ok && !turn.outside() { // the turn waiting ended: stopped
						gone = true
						return
					}
					t := b.task(a.ID)
					if found = t != nil && t != turn.task; !found {
						return
					}
					running, status = started(t), t.Status
					if !running || time.Now().After(deadline) {
						desc = describe(t)
					}
					if !logged && !turn.outside() {
						turn.task.addLog(fmt.Sprintf("⧗ waiting for #%d", t.ID))
						logged = true
					}
				}) {
					return "", fmt.Errorf("the board did not answer")
				}
				switch {
				case gone:
					return "", fmt.Errorf("your turn ended")
				case !found:
					return "", fmt.Errorf("no other task #%d: list_todos shows them", a.ID)
				case !running:
					return fmt.Sprintf("Task #%d is %s.\n\n%s", a.ID, status, desc), nil
				case desc != "":
					return fmt.Sprintf("Task #%d is still %s after %ds; call wait_todo again to wait on.\n\n%s", a.ID, status, a.Timeout, desc), nil
				}
			}
		},
	},
	{
		name: "message_todo",
		description: "Send a message to another task's agent: while it works, it reads it at its next step (or after its turn, for agents that cannot), " +
			"to steer it or give it context it needs. A subtask you filed that has finished takes it as its next turn, to ask it for more. " +
			"Other tasks that wait for the user take no messages, but from outside the board, as from a terminal, any task with a conversation does.",
		schema: object(map[string]any{
			"id":      map[string]any{"type": "integer", "description": "The task's number."},
			"message": str("The message, with the context it needs."),
		}, "id", "message"),
		outside: true,
		run: func(b *board, turn *bridgeTurn, args json.RawMessage) (string, error) {
			var a struct {
				ID      int
				Message string
			}
			if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.Message) == "" {
				return "", fmt.Errorf("message_todo needs an id and a message")
			}
			t, from := b.task(a.ID), turn.task
			if t == nil || t == from {
				return "", fmt.Errorf("no other task #%d: list_todos shows them", a.ID)
			}
			if turn.outside() { // the user's own agent: the user's words
				switch b.say(t, a.Message, "terminal") {
				case saidLive:
					return fmt.Sprintf("Delivered: #%d reads it at its next step.", t.ID), nil
				case saidLater:
					return fmt.Sprintf("Queued: #%d gets it when its turn ends.", t.ID), nil
				case saidTurn:
					return fmt.Sprintf("Sent: #%d is working on it; wait_todo waits for its answer.", t.ID), nil
				}
				return "", fmt.Errorf("task #%d has no conversation to continue: it never ran", t.ID)
			}
			who := fmt.Sprintf("#%d (%s)", from.ID, from.Agent)
			if _, running := b.runs[t.ID]; !running && t.Status != Queued && t.From != from.ID {
				return "", fmt.Errorf("task #%d is %s and waits for the user; only a running task, or a subtask of yours, takes messages", t.ID, t.Status)
			}
			text := "A message from task " + who + ", another agent on the board:\n\n" + strings.TrimSpace(a.Message)
			switch b.say(t, text, who) {
			case saidLive:
				from.addLog(fmt.Sprintf("→ #%d, while it works: %s", t.ID, firstLine(a.Message)))
				return fmt.Sprintf("Delivered: #%d reads it at its next step.", t.ID), nil
			case saidLater:
				from.addLog(fmt.Sprintf("→ #%d, after its turn: %s", t.ID, firstLine(a.Message)))
				return fmt.Sprintf("Queued: #%d gets it when its turn ends.", t.ID), nil
			case saidTurn:
				from.addLog(fmt.Sprintf("→ #%d: %s", t.ID, firstLine(a.Message)))
				return fmt.Sprintf("Sent: #%d is working on it; wait_todo waits for its answer.", t.ID), nil
			}
			return "", fmt.Errorf("task #%d has no conversation to continue", t.ID)
		},
	},
	{
		name:        "report_status",
		description: "Say in a few words what you are doing now, such as which step of how many, or what you wait for: it shows on your task's card while you work.",
		schema:      object(map[string]any{"status": str("A few words, such as \"step 2/4: writing the tests\".")}, "status"),
		run: func(b *board, turn *bridgeTurn, args json.RawMessage) (string, error) {
			var a struct{ Status string }
			json.Unmarshal(args, &a)
			if a.Status = firstLine(a.Status); a.Status == "" {
				return "", fmt.Errorf("report_status needs a status")
			}
			turn.task.Note = clip(a.Status, 120)
			turn.task.addLog("◆ " + turn.task.Note)
			return "Shown on your card.", nil
		},
	},
}

func (turn *bridgeTurn) taskID() int {
	if turn.task == nil {
		return 0
	}
	return turn.task.ID
}

// fileForUser files a task an agent outside the board asks for: the user's,
// through their own agent, so it plans first only if asked to.
func fileForUser(b *board, title, prompt, agent, model, folder string, start, readOnly, planFirst bool) (string, error) {
	folder = expandHome(strings.TrimSpace(folder))
	if !filepath.IsAbs(folder) {
		return "", fmt.Errorf("give the folder to work in, as an absolute path")
	}
	if fi, err := os.Stat(folder); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("no folder %s", folder)
	}
	agent = cmp.Or(agent, "claude")
	if drivers[agent] == nil && agent != anyAgent {
		return "", fmt.Errorf("no agent %q: one of %s", agent, strings.Join(agentChoices, ", "))
	}
	title = cmp.Or(strings.TrimSpace(title), firstLine(prompt))
	b.NextID++
	t := &Task{ID: b.NextID, Title: title, Prompt: strings.TrimSpace(prompt), Agent: agent, Model: strings.TrimSpace(model), Dir: folder, Status: Todo, ReadOnly: readOnly, PlanFirst: planFirst}
	t.addLog("── filed from outside the board, by your agent")
	b.Tasks = append(b.Tasks, t)
	if start {
		b.start(t)
	} else {
		b.save()
		b.autopick()
	}
	return fmt.Sprintf("Filed task #%d for %s: %s (%s). wait_todo waits for it.", t.ID, t.Agent, title, t.Status), nil
}

// waitPoll is how often wait_todo looks at the task it waits for.
var waitPoll = time.Second

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
