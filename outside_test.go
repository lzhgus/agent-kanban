package main

import (
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// An agent outside the board, such as Claude Code in your terminal, files
// tasks for you, waits for them and messages them; it cannot ask on the
// board or report on a card.
func TestOutsideAgents(t *testing.T) {
	g := &gated{gates: map[string]chan struct{}{"build it": make(chan struct{})}}
	g.install(t)
	saved := waitPoll
	waitPoll = 10 * time.Millisecond
	t.Cleanup(func() { waitPoll = saved })
	b, br, drain := bridgeBoard(t)
	url := br.openOutside("tok")
	_, out, _ := mcpPost(url, "tools/list", 1, nil)
	var names []string
	for _, tool := range out["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if strings.Join(names, " ") != "create_todo list_todos get_todo wait_todo message_todo" {
		t.Errorf("tools %v", names)
	}
	if text, isErr := callTool(t, url, "ask_user", map[string]any{"question": "?"}); !isErr {
		t.Errorf("ask_user outside: %q", text)
	}
	if text, isErr := callTool(t, url, "create_todo", map[string]any{"title": "x", "prompt": "build it", "folder": "relative"}); !isErr {
		t.Errorf("a relative folder: %q", text)
	}
	dir := t.TempDir()
	text, isErr := callTool(t, url, "create_todo", map[string]any{"title": "Build", "prompt": "build it", "folder": dir, "start": true, "model": "haiku"})
	task := b.Tasks[len(b.Tasks)-1]
	if isErr || task.Status != Running || task.PlanFirst || task.ByAgent || task.Agent != "claude" || task.Model != "haiku" || task.Dir != dir {
		t.Fatalf("%q: %+v", text, task)
	}
	if text, isErr := callTool(t, url, "message_todo", map[string]any{"id": task.ID, "message": "use tabs"}); isErr || !strings.HasPrefix(text, "Delivered") {
		t.Errorf("to a running task: %q", text)
	}
	close(g.gates["build it"])
	if text, isErr := callTool(t, url, "wait_todo", map[string]any{"id": task.ID}); isErr || !strings.HasPrefix(text, "Task #1 is Review") {
		t.Errorf("wait: %q", text)
	}
	if text, isErr := callTool(t, url, "message_todo", map[string]any{"id": task.ID, "message": "now the docs"}); isErr || !strings.HasPrefix(text, "Sent") {
		t.Errorf("to a task waiting for you: %q", text)
	}
	drain(task)
	if !strings.Contains(strings.Join(task.Log, "\n"), "› terminal: now the docs") {
		t.Errorf("log %q", task.Log)
	}
	if text, isErr := callTool(t, url, "create_todo", map[string]any{"title": "Plan", "prompt": "plan it", "folder": dir, "plan_first": true}); isErr || !b.Tasks[len(b.Tasks)-1].PlanFirst {
		t.Errorf("plan first: %q", text)
	}
}

// A Claude Code session you ran in a terminal becomes a task: a reply
// resumes it, in its own folder, never a worktree.
func TestTakeOver(t *testing.T) {
	r := &resumed{}
	saved := drivers
	drivers = map[string]driver{"claude": r.driver}
	t.Cleanup(func() { drivers = saved })
	b, drain := boardWith(t)
	repo := testRepo(t)
	s := &Session{Agent: "claude", ID: "sess-1", Title: "Fix the flaky test", Dir: repo, Model: "haiku", Updated: time.Now(), Recent: []string{"› fix it", "I fixed it."}}
	b.setSessions([]*Session{s, {Agent: "claude", ID: "busy", Working: true}, {Agent: "codex", ID: "c1"}})
	if b.takeOver(b.sessions[1]) != nil || b.takeOver(b.sessions[2]) != nil {
		t.Error("took over a working session, or one of an agent the board does not run")
	}
	task := b.takeOver(s)
	if task == nil || task.Status != Review || !task.InPlace || task.SessionRef != "sess-1" || b.selected != task || len(b.sessions) != 2 {
		t.Fatalf("%+v, %d sessions", task, len(b.sessions))
	}
	b.setSessions([]*Session{s})
	if len(b.sessions) != 0 {
		t.Error("the watcher still shows it")
	}
	b.reply(task, "and the other test")
	drain(task)
	turn := r.turns[0]
	if !turn.resume || turn.SessionRef != "sess-1" || turn.Dir != repo || turn.Model != "haiku" || task.WS != nil || task.Status != Review {
		t.Errorf("the reply: resume %v %s in %s, ws %v, %s", turn.resume, turn.SessionRef, turn.Dir, task.WS, task.Status)
	}
}

// The sidebar copies the command that adds the board to Claude Code; a
// watched Claude session offers to continue on the board.
func TestTerminalViews(t *testing.T) {
	b, _ := boardWith(t)
	var copied string
	b.copyText = func(s string) { copied = s }
	b.outsideURL = "http://127.0.0.1:47821/mcp/tok"
	s := &Session{Agent: "claude", ID: "sess-1", Title: "Outside work", Dir: t.TempDir(), Updated: time.Now()}
	b.setSessions([]*Session{s})
	b.selectedSession = s.key()
	tt := ui.NewTester(b.view, 1440, 880)
	tt.Frame()
	if err := tt.Click("Use the board from your terminal"); err != nil {
		t.Fatal(err)
	}
	if copied != "claude mcp add --transport http --scope user agent-kanban http://127.0.0.1:47821/mcp/tok" {
		t.Errorf("copied %q", copied)
	}
	tt.Frame()
	if err := tt.Click("Continue on the board"); err != nil {
		t.Fatal(err)
	}
	if len(b.Tasks) != 1 || b.Tasks[0].SessionRef != "sess-1" {
		t.Errorf("tasks %+v", b.Tasks)
	}
}
