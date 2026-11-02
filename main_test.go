package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

func collect() (*[]string, emit) {
	var lines []string
	return &lines, func(l string) { lines = append(lines, l) }
}

// The shapes below are what `claude -p --output-format stream-json --verbose`
// printed (Claude Code 2.1.316).
func TestClaudeLines(t *testing.T) {
	lines, log := collect()
	var r result
	for _, l := range []string{
		`{"type":"system","subtype":"init","session_id":"s-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Looking."},{"type":"tool_use","name":"Bash","input":{"command":"go test ./...\nmore"}}]},"parent_tool_use_id":null}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"subagent noise"}]},"parent_tool_use_id":"toolu_1"}`,
		`not json`,
		`{"type":"result","subtype":"success","is_error":false,"result":"hi","total_cost_usd":0.0014,"modelUsage":{"claude-haiku-5-5":{"inputTokens":2,"outputTokens":4,"cacheReadInputTokens":11382,"cacheCreationInputTokens":6624}}}`,
	} {
		claudeLine([]byte(l), &r, log)
	}
	want := []string{"Looking.", "▸ Bash go test ./..."}
	if fmt.Sprint(*lines) != fmt.Sprint(want) {
		t.Errorf("log %q, want %q", *lines, want)
	}
	if r.Text != "hi" || r.Err != "" || r.SessionRef != "s-1" || r.Usage != (Usage{Input: 2, Output: 4, CacheRead: 11382, CacheWrite: 6624, CostUSD: 0.0014}) {
		t.Errorf("result %+v", r)
	}
	claudeLine([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":""}`), &r, log)
	if !strings.Contains(r.Err, "error_max_turns") {
		t.Errorf("error %q", r.Err)
	}
}

// The event shapes `agy --output-format stream-json` prints.
func TestAntigravityLines(t *testing.T) {
	lines, log := collect()
	var r result
	s := &agyStream{counted: map[int]bool{}, announced: map[int]bool{}}
	ended := false
	for _, l := range []string{
		`{"event":"init","conversation_id":"c1"}`,
		`{"event":"step_update","step_update":{"step_type":"tool","step_index":1,"state":"ACTIVE","tool_name":"run_command","tool_info":{"parameters":{"command":"ls"}}}}`,
		`{"event":"step_update","step_update":{"step_type":"tool","step_index":1,"state":"DONE","tool_name":"run_command","usage":{"input_tokens":100,"output_tokens":10,"cache_read_tokens":50}}}`,
		`{"event":"step_update","step_update":{"step_type":"agent_response","step_index":2,"state":"ACTIVE","text_delta":"Al"}}`,
		`{"event":"step_update","step_update":{"step_type":"agent_response","step_index":2,"state":"DONE","text_delta":"l done","usage":{"input_tokens":200,"output_tokens":20}}}`,
		`{"event":"step_update","step_update":{"step_type":"agent_response","step_index":2,"state":"DONE","usage":{"input_tokens":200,"output_tokens":20}}}`,
		`{"event":"result","result":{}}`,
	} {
		ended = s.line([]byte(l), &r, log) || ended
	}
	want := []string{"▸ run_command ls", "All done"}
	if fmt.Sprint(*lines) != fmt.Sprint(want) {
		t.Errorf("log %q, want %q", *lines, want)
	}
	if !ended || r.Text != "All done" || r.SessionRef != "c1" || r.Usage != (Usage{Input: 300, Output: 30, CacheRead: 50}) {
		t.Errorf("ended %v, result %+v", ended, r)
	}
}

// A first turn names the session; a reply resumes it.
func TestSessionArgs(t *testing.T) {
	task := Task{Prompt: "go", SessionRef: "abc"}
	if a := strings.Join(claudeArgs(task), " "); !strings.Contains(a, "--session-id abc") || strings.Contains(a, "--resume") {
		t.Errorf("claude, first turn: %s", a)
	}
	if a := strings.Join(agyArgs(task), " "); strings.Contains(a, "--conversation") {
		t.Errorf("agy, first turn: %s", a)
	}
	task.resume = true
	if a := strings.Join(claudeArgs(task), " "); !strings.Contains(a, "--resume abc") || strings.Contains(a, "--session-id") {
		t.Errorf("claude, reply: %s", a)
	}
	if a := strings.Join(agyArgs(task), " "); !strings.Contains(a, "--conversation abc") {
		t.Errorf("agy, reply: %s", a)
	}
	if a := strings.Join(claudeArgs(task), " "); !strings.Contains(a, "--disallowedTools=mcp__agent-kanban") || strings.Contains(a, "Write") {
		t.Errorf("claude, a building turn denies only the outside server: %s", a)
	}
	task.readOnly = true
	if a := strings.Join(claudeArgs(task), " "); !strings.Contains(a, "--disallowedTools=Write,Edit,NotebookEdit") {
		t.Errorf("claude, a planning turn: %s", a)
	}
}

// The session events `copilot --headless --stdio` sends.
func TestCopilotEvents(t *testing.T) {
	lines, log := collect()
	var r result
	var s copilotStream
	ev := func(typ, data string) bool {
		return s.event(json.RawMessage(`{"event":{"type":"`+typ+`","data":`+data+`}}`), &r, log)
	}
	if ev("session.idle", `{}`) {
		t.Fatal("an idle before the turn ended it")
	}
	s.started = true
	ev("assistant.message_delta", `{"messageId":"m1","deltaContent":"Fix"}`)
	ev("assistant.message_delta", `{"messageId":"m1","deltaContent":"ing"}`)
	ev("tool.execution_start", `{"toolName":"bash","arguments":{"command":"make"}}`)
	ev("assistant.message", `{"messageId":"m2","content":"Fixed it."}`)
	ev("assistant.usage", `{"model":"gpt-5","inputTokens":1000,"outputTokens":50,"cacheReadTokens":600,"cacheWriteTokens":100}`)
	s.event(json.RawMessage(`{"event":{"type":"assistant.message","agentId":"sub","data":{"content":"subagent"}}}`), &r, log)
	if !ev("session.idle", `{}`) {
		t.Fatal("idle did not end the turn")
	}
	want := []string{"Fixing", "▸ bash make", "Fixed it."}
	if fmt.Sprint(*lines) != fmt.Sprint(want) {
		t.Errorf("log %q, want %q", *lines, want)
	}
	if r.Text != "Fixed it." || r.Usage != (Usage{Input: 300, Output: 50, CacheRead: 600, CacheWrite: 100}) {
		t.Errorf("result %+v", r)
	}
}

// A stopped turn rests where the task was: a first one in To Do, a reply in
// Review with its conversation kept, though the killed agent reports its kill.
func TestStop(t *testing.T) {
	saved := drivers
	drivers = map[string]driver{"claude": func(ctx context.Context, task Task, _ emit) (result, error) {
		<-ctx.Done()
		return result{SessionRef: task.SessionRef}, errors.New("signal: killed")
	}}
	defer func() { drivers = saved }()
	queue := make(chan func(), 16)
	b := newBoard("")
	b.update = func(f func()) { queue <- f }
	b.dir, b.prompt = t.TempDir(), "loop forever"
	b.add(true)
	task := b.Tasks[0]
	b.stop(task)
	for task.Status == Running {
		(<-queue)()
	}
	if task.Status != Todo || task.Err != "" || len(b.runs) != 0 {
		t.Errorf("after stopping the first turn: status %q, err %q, runs %d", task.Status, task.Err, len(b.runs))
	}
	task.Status = Review
	session := task.SessionRef
	b.reply(task, "and more")
	b.stop(task)
	for task.Status == Running {
		(<-queue)()
	}
	if task.Status != Review || task.SessionRef != session || session == "" {
		t.Errorf("after stopping a reply: status %q, session %q (was %q)", task.Status, task.SessionRef, session)
	}
}

func TestFrames(t *testing.T) {
	var buf bytes.Buffer
	for _, body := range []string{`{"a":1}`, `{"b":"é"}`} {
		fmt.Fprintf(&buf, "Content-Length: %d\r\nContent-Type: x\r\n\r\n%s", len(body), body)
	}
	br := bufio.NewReader(&buf)
	for _, want := range []string{`{"a":1}`, `{"b":"é"}`} {
		got, err := readFrame(br)
		if err != nil || string(got) != want {
			t.Fatalf("frame %q, %v; want %q", got, err, want)
		}
	}
	if _, err := readFrame(br); err == nil {
		t.Error("read past the end")
	}
}

// The board runs a card through a fake agent, continues its conversation,
// and hands it off to another.
func TestBoard(t *testing.T) {
	ran := map[string]string{}
	got := map[string]Task{}
	fake := func(name string) driver {
		return func(ctx context.Context, task Task, log emit) (result, error) {
			ran[name], got[name] = task.Prompt, task
			log("working")
			session := task.SessionRef // Claude reports the id it was given
			if session == "" {
				session = "conv-" + name
			}
			return result{Text: name + " finished", Usage: Usage{Input: 10, Output: 5}, SessionRef: session}, nil
		}
	}
	saved := drivers
	drivers = map[string]driver{"claude": fake("claude"), "antigravity": fake("antigravity"), "copilot": fake("copilot")}
	defer func() { drivers = saved }()

	queue := make(chan func(), 16)
	b := newBoard("")
	b.update = func(f func()) { queue <- f }
	b.dir = t.TempDir()
	tt := ui.NewTester(b.view, 1400, 860)

	tt.Frame()
	if err := tt.Click("New task"); err != nil {
		t.Fatal(err)
	}
	b.prompt = "Add a README"
	tt.Frame()
	if err := tt.Click("Add & Run"); err != nil {
		t.Fatal(err)
	}
	task := b.Tasks[0]
	for task.Status == Running {
		(<-queue)()
	}
	if task.Status != Review || task.Result != "claude finished" || task.Usage.Input != 10 || !strings.Contains(strings.Join(task.Log, "\n"), "working") {
		t.Fatalf("after the run: %+v", task)
	}
	session := task.SessionRef
	if session == "" || got["claude"].resume || len(task.SessionIDs) != 1 || task.SessionIDs[0] != session {
		t.Fatalf("the first turn's session: %q, %+v", session, got["claude"])
	}

	// A reply continues the same conversation.
	tt.Frame()
	task.draft = "Also add a license"
	tt.Frame()
	if err := tt.Click("Send"); err != nil {
		t.Fatal(err)
	}
	for task.Status == Running {
		(<-queue)()
	}
	if c := got["claude"]; !c.resume || c.SessionRef != session || c.Prompt != "Also add a license" {
		t.Errorf("the reply ran %+v", c)
	}
	if task.Status != Review || task.Runs != 2 || task.SessionRef != session || task.draft != "" || task.Usage.Input != 20 ||
		!strings.Contains(strings.Join(task.Log, "\n"), "› Also add a license") {
		t.Errorf("after the reply: %+v", task)
	}

	tt.Frame()
	if err := tt.Click("Hand off"); err != nil {
		t.Fatal(err)
	}
	next := b.Tasks[1]
	for next.Status == Running {
		(<-queue)()
	}
	if next.Agent != "antigravity" || next.From != task.ID || !strings.Contains(ran["antigravity"], "claude finished") {
		t.Errorf("handoff: %+v, prompt %q", next, ran["antigravity"])
	}
	tt.Frame()
	if next.SessionRef != "conv-antigravity" {
		t.Errorf("antigravity's conversation: %q", next.SessionRef)
	}
	if !tt.HasText("45 tokens") || !tt.HasText("30 tokens") || !tt.HasText("15 tokens") { // all agents, claude's two turns, antigravity's one
		t.Errorf("usage missing from %q", tt.Texts())
	}
	if err := tt.Click("antigravity"); err != nil { // the sidebar filters the board
		t.Fatal(err)
	}
	tt.Frame()
	if _, claudes := tt.Find("Add a README"); claudes || !tt.HasText("↪ Add a README") {
		t.Errorf("the filter shows %q", tt.Texts())
	}
}

// Watched sessions: at work, waiting for you, done once idle 2 hours; a
// drag holds until the session goes on.
func TestSessionStatus(t *testing.T) {
	b := newBoard("")
	b.path = ""
	now := time.Now()
	s := &Session{Agent: "codex", ID: "a", Updated: now.Add(-time.Hour)}
	for _, c := range []struct {
		working bool
		updated time.Duration
		want    Status
	}{{true, 0, Running}, {false, time.Hour, Review}, {false, 3 * time.Hour, Done}} {
		s.Working, s.Updated = c.working, now.Add(-c.updated)
		if got := b.sessionStatus(s); got != c.want {
			t.Errorf("working %v, idle %v: %s, want %s", c.working, c.updated, got, c.want)
		}
	}
	b.moveSession(s, Review) // idle 3 hours, kept waiting by hand
	if got := b.sessionStatus(s); got != Review {
		t.Errorf("dragged to Review: %s", got)
	}
	s.Updated = now // it goes on: the drag no longer holds
	if got := b.sessionStatus(s); got != Review {
		t.Errorf("after new activity: %s", got)
	}
	b.moveSession(s, Done)
	if got := b.sessionStatus(s); got != Done {
		t.Errorf("dragged to Done: %s", got)
	}
	s.Updated = now.Add(time.Second)
	if got := b.sessionStatus(s); got != Review {
		t.Errorf("a dismissed session that goes on: %s", got)
	}
	b.moveSession(s, Running)
	if got := b.sessionStatus(s); got != Review {
		t.Errorf("dragged to Running: %s", got)
	}
}

// Opened from Finder, the app takes the login shell's PATH, whatever the
// shell's startup prints around it.
func TestShellPath(t *testing.T) {
	noisy := "[WARN] - (starship::utils): Executing command timed out.\n\n" + pathMarker + "/x/bin:/usr/bin" + pathMarker + "\n% "
	if p := markedPath([]byte(noisy)); p != "/x/bin:/usr/bin" {
		t.Errorf("markedPath %q", p)
	}
	if p := markedPath([]byte("no markers")); p != "" {
		t.Errorf("markedPath without markers %q", p)
	}
	if _, err := os.Stat("/bin/zsh"); err != nil {
		t.Skip("no zsh")
	}
	t.Setenv("TERM", "")
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/nowhere")
	useShellPath()
	if p := os.Getenv("PATH"); !strings.HasSuffix(p, ":/nowhere") || !strings.Contains(p, "/usr/bin") {
		t.Errorf("PATH %q", p)
	}
}
