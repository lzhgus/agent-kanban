package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Claude driver takes messages while it works: one that comes during
// the work is folded into it; one that comes after a result runs on in the
// same process; usage counts the process once.
func TestClaudeLiveProtocol(t *testing.T) {
	calls := fakeClaudeCLI(t)
	dir := t.TempDir()
	run := func(prompt string, early ...string) (result, chan string) {
		inbox := make(chan string, 4)
		for _, m := range early {
			inbox <- m
		}
		r, err := runClaude(context.Background(), Task{Agent: "claude", Prompt: prompt, Dir: dir, SessionRef: "s1", inbox: inbox}, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		return r, inbox
	}

	r, _ := run("build it", "also test it")
	if r.Text != "got: build it | also test it" || r.Usage.Input != 10 || r.SessionRef != "fake-session" {
		t.Errorf("folded in: %+v", r)
	}
	if args := readLog(t, calls)[0]; !strings.Contains(args, "--input-format stream-json") || !strings.Contains(args, "--replay-user-messages") || strings.Contains(args, "build it") {
		t.Errorf("args %q", args)
	}

	r, _ = run("quick one", "two") // the first work ends before it takes "two" in
	if r.Text != "got: two" || r.Usage.Input != 20 || r.Usage.CostUSD != 0.02 {
		t.Errorf("after a result: %+v", r)
	}
	readLog(t, calls)

	inbox := make(chan string, 4)
	r, err := runClaude(context.Background(), Task{Agent: "claude", Prompt: "quick", Dir: dir, SessionRef: "s1", inbox: inbox}, func(string) {})
	inbox <- "too late" // the turn is over: the board hands it to the next
	if err != nil || r.Text != "got: quick" || len(inbox) != 1 {
		t.Errorf("%v %+v, %d left", err, r, len(inbox))
	}
}

// listening is a driver that, for a live agent, reads one message from the
// turn's inbox before it ends; others wait for their gate.
type listening struct {
	mu    sync.Mutex
	turns []Task
	gate  chan struct{}
}

func (l *listening) driver(ctx context.Context, task Task, log emit) (result, error) {
	l.mu.Lock()
	l.turns = append(l.turns, task)
	l.mu.Unlock()
	text := "done"
	l.mu.Lock()
	gate := l.gate
	l.mu.Unlock()
	if task.inbox != nil && strings.HasPrefix(task.Prompt, "build") {
		select {
		case m := <-task.inbox:
			text = "got " + m
		case <-ctx.Done():
			return result{}, ctx.Err()
		}
	} else if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return result{}, ctx.Err()
		}
	}
	return result{Text: text, SessionRef: cmpSession(task)}, nil
}

func cmpSession(task Task) string {
	if task.SessionRef != "" {
		return task.SessionRef
	}
	return "conv-" + task.Agent
}

// What you tell an agent while it works reaches it: at its next step if it
// takes messages then, else as its next turn; a stop keeps it for your
// next reply.
func TestTellWhileRunning(t *testing.T) {
	l := &listening{gate: make(chan struct{})}
	saved := drivers
	drivers = map[string]driver{"claude": l.driver, "copilot": l.driver}
	t.Cleanup(func() { drivers = saved })
	b, drain := boardWith(t)
	dir := t.TempDir()

	live := addTask(b, dir, "build")
	if got := b.say(live, "use tabs", ""); got != saidLive {
		t.Fatalf("live: %q", got)
	}
	drain(live)
	if live.Result != "got use tabs" || live.Status != Review || len(live.Asides) != 0 {
		t.Errorf("live: %q %s %v", live.Result, live.Status, live.Asides)
	}

	late := addTask(b, dir, "ignore the inbox") // it never reads it: the message waits for its next turn
	b.say(late, "and a README", "")
	close(l.gate)
	drain(late)
	for late.Status == Running {
		(<-queue)()
	}
	last := l.turns[len(l.turns)-1]
	if last.ID != late.ID || !strings.Contains(last.Prompt, "and a README") || !last.resume || late.Status != Review || len(late.Asides) != 0 {
		t.Errorf("late: %q %s %v", last.Prompt, late.Status, late.Asides)
	}

	b.agent = "copilot"
	b.dir, b.prompt = dir, "ignore: copilot"
	b.add(true)
	other := b.Tasks[len(b.Tasks)-1]
	drain(other) // a first turn: now it has a conversation to go on with
	l.mu.Lock()
	gate := make(chan struct{})
	l.gate = gate
	l.mu.Unlock()
	b.reply(other, "next step")
	if got := b.say(other, "smaller", ""); got != saidLater || len(other.Asides) != 1 {
		t.Fatalf("copilot: %q %v", got, other.Asides)
	}
	b.stop(other)
	drain(other)
	if len(other.Asides) != 1 {
		t.Fatalf("a stop lost the message: %v", other.Asides)
	}
	b.reply(other, "go on")
	close(gate)
	drain(other)
	last = l.turns[len(l.turns)-1]
	if last.Prompt != "smaller\n\ngo on" || len(other.Asides) != 0 {
		t.Errorf("held message: %q %v", last.Prompt, other.Asides)
	}
}

// An agent hands part of its task to another as a subtask, tells it
// something while it works, waits for it and asks it for more; other
// tasks waiting for you take no messages.
func TestSubtasks(t *testing.T) {
	g := &gated{gates: map[string]chan struct{}{"lead it": make(chan struct{}), "the parser": make(chan struct{}), "slow": make(chan struct{})}}
	g.install(t)
	saved := waitPoll
	waitPoll = 10 * time.Millisecond
	t.Cleanup(func() { waitPoll = saved })
	b, br, drain := bridgeBoard(t)
	b.MaxRunning = 1
	repo := testRepo(t)
	lead := addTask(b, repo, "lead it")
	for lead.WS == nil {
		(<-queue)()
	}
	idle := addTask(b, repo, "idle") // queued: one slot
	url, closeTurn := br.open(lead, false)
	defer closeTurn()

	text, isErr := callTool(t, url, "create_todo", map[string]any{"title": "Parser", "prompt": "the parser", "agent": "claude", "subtask": true})
	sub := b.Tasks[len(b.Tasks)-1]
	if isErr || !sub.Sub || sub.PlanFirst || sub.Status != Running || sub.From != lead.ID || sub.Dir != lead.WS.Dir() {
		t.Fatalf("%q: sub %+v", text, sub)
	}
	for sub.WS == nil {
		(<-queue)()
	}
	if sub.WS.Base != lead.WS.Branch {
		t.Errorf("the subtask forked from %q, not %q", sub.WS.Base, lead.WS.Branch)
	}
	if text, _ := callTool(t, url, "report_status", map[string]any{"status": "waiting for the parser"}); lead.Note != "waiting for the parser" {
		t.Errorf("%q: note %q", text, lead.Note)
	}
	if text, isErr := callTool(t, url, "message_todo", map[string]any{"id": sub.ID, "message": "use a table"}); isErr || !strings.HasPrefix(text, "Delivered") {
		t.Errorf("to the running subtask: %q", text)
	}
	if text, isErr := callTool(t, url, "wait_todo", map[string]any{"id": sub.ID, "timeout_seconds": 1}); isErr || !strings.Contains(text, "still Running") {
		t.Errorf("timed out: %q", text)
	}
	close(g.gates["the parser"])
	text, isErr = callTool(t, url, "wait_todo", map[string]any{"id": sub.ID})
	if isErr || !strings.HasPrefix(text, "Task #3 is Review") || !strings.Contains(text, "branch: "+sub.WS.Branch) {
		t.Errorf("waited: %q", text)
	}
	g.mu.Lock()
	if !strings.Contains(g.ran[len(g.ran)-1], "use a table") { // it never read it while it worked: its next turn
		t.Errorf("the message was not its next turn: %q", g.ran)
	}
	g.mu.Unlock()
	if text, isErr := callTool(t, url, "message_todo", map[string]any{"id": sub.ID, "message": "add tests"}); isErr || !strings.HasPrefix(text, "Sent") {
		t.Errorf("more from the finished subtask: %q", text)
	}
	drain(sub)
	b.stop(idle)
	if text, isErr := callTool(t, url, "message_todo", map[string]any{"id": idle.ID, "message": "hi"}); !isErr {
		t.Errorf("a task waiting for you took a message: %q", text)
	}
	if text, isErr := callTool(t, url, "create_todo", map[string]any{"title": "Notes", "prompt": "read the code", "read_only": true, "start": true}); isErr {
		t.Errorf("%q", text)
	}
	if notes := b.Tasks[len(b.Tasks)-1]; !notes.ReadOnly || !notes.PlanFirst || notes.Sub {
		t.Errorf("a separate task: %+v", notes)
	}
	close(g.gates["lead it"])
	drain(lead)
}
