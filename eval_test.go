//go:build eval

package main

// End-to-end checks against the real Claude Code CLI, on a scratch git
// repository: they spend a little of your plan, so they only run when asked:
//
//	go test -tags eval -run TestEval -v -timeout 15m
//
// EVAL_MODEL picks the model (default haiku).

import (
	"cmp"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui/editor"
)

// evalBoard is a board as the app runs it, with its MCP server, saved to
// path, its window in the background.
func evalBoard(t *testing.T, path, worktrees string) (*board, chan func(), *watching) {
	t.Helper()
	main := make(chan func(), 1024) // what runs on the board's main thread
	b := newBoard(path)
	b.worktrees = worktrees
	b.update = func(f func()) { main <- f }
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	br := newBridge(b)
	br.base = "http://" + ln.Addr().String() + "/mcp/"
	go http.Serve(ln, br)
	b.bridge = br
	w := &watching{}
	w.install(b)
	return b, main, w
}

// pump runs the board's main thread until done holds.
func pump(t *testing.T, main chan func(), done func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for !done() {
		select {
		case f := <-main:
			f()
		case <-deadline:
			t.Fatal("timed out")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func settledOf(task *Task) func() bool { return func() bool { return !started(task) } }

func evalRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("no claude on PATH")
	}
	return testRepo(t)
}

func evalTask(b *board, dir, prompt string) *Task {
	b.dir, b.prompt, b.agent, b.model = dir, prompt, "claude", cmp.Or(os.Getenv("EVAL_MODEL"), "haiku")
	b.add(true)
	return b.Tasks[len(b.Tasks)-1]
}

func logOf(task *Task) string { return strings.Join(task.Log, "\n") }

func read(path string) string {
	data, _ := os.ReadFile(path)
	return strings.TrimSpace(string(data))
}

// A turn's work lands in Review on its branch, unread, announced.
func TestEvalTurnNotifies(t *testing.T) {
	repo := evalRepo(t)
	b, main, w := evalBoard(t, "", t.TempDir())
	task := evalTask(b, repo, "Create a file hello.txt containing exactly the word hi. Then reply with one short sentence.")
	pump(t, main, settledOf(task), 5*time.Minute)
	t.Logf("log:\n%s", logOf(task))
	if task.Status != Review || task.WS == nil || read(filepath.Join(task.WS.Dir(), "hello.txt")) != "hi" {
		t.Fatalf("%s %q", task.Status, task.Err)
	}
	if task.Changes.Files != 1 || !task.Unread || len(w.notices) != 1 || !strings.HasPrefix(w.notices[0].Body, "Done: ") {
		t.Errorf("changes %+v, unread %v, notices %+v", task.Changes, task.Unread, w.notices)
	}
}

// The agent asks you through the board; the notification carries the
// question; your answer is its next message.
func TestEvalAskUser(t *testing.T) {
	repo := evalRepo(t)
	b, main, w := evalBoard(t, "", t.TempDir())
	task := evalTask(b, repo, "Use the ask_user tool of the kanban MCP server to ask me which color I want, red or blue, then end your turn. "+
		"Once I answer, write my answer, one word, to color.txt.")
	pump(t, main, settledOf(task), 5*time.Minute)
	t.Logf("log:\n%s", logOf(task))
	if task.Question == nil || len(w.notices) != 1 || !strings.HasPrefix(w.notices[0].Body, "Asks: ") {
		t.Fatalf("question %+v, notices %+v", task.Question, w.notices)
	}
	b.reply(task, "blue")
	pump(t, main, settledOf(task), 5*time.Minute)
	t.Logf("log:\n%s", logOf(task))
	if task.Status != Review || read(filepath.Join(task.WS.Dir(), "color.txt")) != "blue" {
		t.Errorf("%s %q: color.txt %q", task.Status, task.Err, read(filepath.Join(task.WS.Dir(), "color.txt")))
	}
}

// The app quits midway through a turn; opened again, the board resumes the
// agent's conversation and the agent finishes the job.
func TestEvalResumeAfterQuit(t *testing.T) {
	repo := evalRepo(t)
	path, worktrees := filepath.Join(t.TempDir(), "board.json"), t.TempDir()
	b, main, _ := evalBoard(t, path, worktrees)
	task := evalTask(b, repo, "Create three files, one at a time: a.txt, b.txt and c.txt, each containing its own letter. "+
		"Before creating each file, run `sleep 4` with the Bash tool. Then reply with one short sentence.")
	pump(t, main, func() bool { return task.WS != nil && read(filepath.Join(task.WS.Dir(), "a.txt")) == "a" }, 5*time.Minute)
	if read(filepath.Join(task.WS.Dir(), "c.txt")) == "c" {
		t.Fatal("it finished before the app could quit")
	}
	b.runs[task.ID]() // the app quits: the agent is killed, and the turn's end is never recorded
	b.procs.Wait()    // its main thread is gone: what the turn left is committed, unrecorded
	t.Logf("quit midway:\n%s", logOf(task))

	b2, main2, w := evalBoard(t, path, worktrees)
	if len(b2.interrupted) != 1 {
		t.Fatalf("%d to resume", len(b2.interrupted))
	}
	again := b2.interrupted[0]
	b2.resumeInterrupted()
	pump(t, main2, settledOf(again), 5*time.Minute)
	t.Logf("resumed:\n%s", logOf(again))
	dir := again.WS.Dir()
	if again.Status != Review || read(filepath.Join(dir, "a.txt")) != "a" || read(filepath.Join(dir, "b.txt")) != "b" || read(filepath.Join(dir, "c.txt")) != "c" {
		t.Errorf("%s %q", again.Status, again.Err)
	}
	if again.SessionRef != task.SessionRef || !again.Unread || len(w.notices) != 1 {
		t.Errorf("conversation %s → %s, unread %v, notices %d", task.SessionRef, again.SessionRef, again.Unread, len(w.notices))
	}
}

// You tell the agent something while it works: it takes it in at its next
// step, in the same turn.
func TestEvalTellWhileRunning(t *testing.T) {
	repo := evalRepo(t)
	b, main, _ := evalBoard(t, "", t.TempDir())
	task := evalTask(b, repo, "Run `sleep 3` with the Bash tool, three separate times, one call each. Then create done.txt containing done. Reply with one short sentence.")
	pump(t, main, func() bool { return strings.Contains(logOf(task), "▸ Bash") }, 5*time.Minute)
	if got := b.say(task, "Also create extra.txt containing extra.", ""); got != saidLive {
		t.Fatalf("said %q", got)
	}
	pump(t, main, settledOf(task), 5*time.Minute)
	t.Logf("log:\n%s", logOf(task))
	dir := task.WS.Dir()
	if task.Status != Review || read(filepath.Join(dir, "done.txt")) != "done" || read(filepath.Join(dir, "extra.txt")) != "extra" {
		t.Errorf("%s %q", task.Status, task.Err)
	}
	if task.Runs != 1 || len(task.Asides) != 0 {
		t.Errorf("%d turns, asides %v", task.Runs, task.Asides)
	}
}

// A lead hands part of its task to a subtask, waits for it and merges its
// branch into its own.
func TestEvalLead(t *testing.T) {
	repo := evalRepo(t)
	b, main, _ := evalBoard(t, "", t.TempDir())
	b.lead = true
	lead := evalTask(b, repo, "In this Go module, add two functions in package main. "+
		"Hand this part to a subtask for agent claude: create app/sub.go with `func Sub(a, b int) int`. "+
		"Meanwhile write app/add.go with `func Add(a, b int) int` yourself, and commit it. "+
		"Then wait for the subtask, merge its branch into yours, run `go build ./...`, and report.")
	pump(t, main, func() bool {
		if started(lead) {
			return false
		}
		for _, x := range b.Tasks {
			if x != lead && started(x) {
				return false
			}
		}
		return true
	}, 10*time.Minute)
	t.Logf("lead:\n%s", logOf(lead))
	var sub *Task
	for _, x := range b.Tasks {
		if x.Sub && x.From == lead.ID {
			sub = x
			t.Logf("subtask #%d:\n%s", x.ID, logOf(x))
		}
	}
	if sub == nil {
		t.Fatal("no subtask")
	}
	dir := lead.WS.Dir()
	if lead.Status != Review || sub.Status != Review || !strings.Contains(read(filepath.Join(dir, "app", "sub.go")), "func Sub") ||
		!strings.Contains(read(filepath.Join(dir, "app", "add.go")), "func Add") {
		t.Errorf("lead %s %q, sub %s %q", lead.Status, lead.Err, sub.Status, sub.Err)
	}
	if !strings.Contains(logOf(lead), "mcp__kanban__wait_todo") || sub.WS == nil || sub.WS.Base != lead.WS.Branch {
		t.Error("the lead did not wait for its subtask, or the subtask did not branch from it")
	}
}

// Claude Code in your terminal files a task on the board through its MCP
// server, waits for it and reads the outcome.
func TestEvalOutsideAgent(t *testing.T) {
	repo := evalRepo(t)
	b, main, _ := evalBoard(t, "", t.TempDir())
	url := b.bridge.openOutside("eval-token")
	model := cmp.Or(os.Getenv("EVAL_MODEL"), "haiku")
	cfg := `{"mcpServers":{"` + outsideName + `":{"type":"http","url":"` + url + `"}}}`
	prompt := "Use the " + outsideName + " MCP server: call create_todo with title \"Hello file\", prompt \"Create hello.txt containing exactly hi.\", " +
		"agent claude, model " + model + ", folder " + repo + ", and start true. Then call wait_todo on the task it filed, and reply with the task's number and status only."
	cmd := exec.Command("claude", "-p", prompt, "--model", model, "--permission-mode", "bypassPermissions", "--mcp-config", cfg, "--strict-mcp-config")
	cmd.Dir = t.TempDir()
	type outcome struct {
		out []byte
		err error
	}
	done := make(chan outcome, 1)
	go func() { out, err := cmd.CombinedOutput(); done <- outcome{out, err} }()
	var got outcome
	pump(t, main, func() bool {
		select {
		case got = <-done:
			return true
		default:
			return false
		}
	}, 8*time.Minute)
	t.Logf("terminal agent: %v\n%s", got.err, got.out)
	if len(b.Tasks) != 1 {
		t.Fatalf("%d tasks", len(b.Tasks))
	}
	task := b.Tasks[0]
	t.Logf("task:\n%s", logOf(task))
	if task.Status != Review || task.WS == nil || read(filepath.Join(task.WS.Dir(), "hello.txt")) != "hi" || task.PlanFirst {
		t.Errorf("%s %q", task.Status, task.Err)
	}
	if !strings.Contains(string(got.out), "Review") {
		t.Errorf("the terminal agent did not read the outcome")
	}
}

// A session run in a terminal, taken over: a reply on the board resumes it
// with what it knew.
func TestEvalTakeOver(t *testing.T) {
	repo := evalRepo(t)
	b, main, _ := evalBoard(t, "", t.TempDir())
	model := cmp.Or(os.Getenv("EVAL_MODEL"), "haiku")
	id := newUUID()
	cmd := exec.Command("claude", "-p", "Remember this code word for later: pelican. Reply with OK only.", "--session-id", id, "--model", model)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	task := b.takeOver(&Session{Agent: "claude", ID: id, Title: "Code word", Dir: repo, Model: model, Updated: time.Now()})
	b.reply(task, "What was the code word? Reply with the word only.")
	pump(t, main, settledOf(task), 5*time.Minute)
	t.Logf("log:\n%s", logOf(task))
	if task.Status != Review || !strings.Contains(strings.ToLower(task.Result), "pelican") || task.WS != nil || task.SessionRef != id {
		t.Errorf("%s %q: %q", task.Status, task.Err, task.Result)
	}
}

// Undo takes the last turn back: its files go, and it is out of the
// agent's conversation. (What the agent did outside the worktree, such as
// Claude Code's own memory, stays.)
func TestEvalUndo(t *testing.T) {
	repo := evalRepo(t)
	b, main, _ := evalBoard(t, "", t.TempDir())
	task := evalTask(b, repo, "Create a.txt containing apple. Reply with one short sentence.")
	pump(t, main, settledOf(task), 5*time.Minute)
	b.reply(task, "Create b.txt containing banana. Reply with one short sentence.")
	pump(t, main, settledOf(task), 5*time.Minute)
	dir := task.WS.Dir()
	if read(filepath.Join(dir, "b.txt")) != "banana" {
		t.Fatalf("turn 2 did not write b.txt: %q", task.Err)
	}
	b.undo(task)
	pump(t, main, func() bool { return !task.undoing }, time.Minute)
	b.reply(task, "Without running any tool, quote the message I sent you just before this one, word for word.")
	pump(t, main, settledOf(task), 5*time.Minute)
	t.Logf("log:\n%s", logOf(task))
	if read(filepath.Join(dir, "b.txt")) != "" || read(filepath.Join(dir, "a.txt")) != "apple" {
		t.Error("the files are not as before turn 2")
	}
	if answer := task.Result; !strings.Contains(answer, "a.txt") || strings.Contains(answer, "b.txt") {
		t.Errorf("the message before: %q", answer)
	}
}

// A schedule that came due while the app was closed runs once; reverting
// its change in the Changes tab takes it off the branch, and the agent's
// next turn finds it gone.
func TestEvalScheduleRevert(t *testing.T) {
	repo := evalRepo(t)
	b, main, _ := evalBoard(t, "", t.TempDir())
	b.dir, b.prompt, b.agent, b.model = repo, "Create a file note.txt containing exactly the word banana. Then reply with one short sentence.", "claude", cmp.Or(os.Getenv("EVAL_MODEL"), "haiku")
	b.repeat, b.at = "Every day", "09:00"
	b.schedule(time.Now().Add(-72 * time.Hour))
	b.fireDue(time.Now())
	if len(b.Tasks) != 1 {
		t.Fatalf("%d tasks", len(b.Tasks))
	}
	task := b.Tasks[0]
	pump(t, main, settledOf(task), 5*time.Minute)
	if task.Status != Review || read(filepath.Join(task.WS.Dir(), "note.txt")) != "banana" || !b.Schedules[0].Next.After(time.Now()) {
		t.Fatalf("%s %q:\n%s", task.Status, task.Err, logOf(task))
	}
	before, after, err := task.WS.texts("note.txt")
	if err != nil {
		t.Fatal(err)
	}
	d := &diffPane{task: task, path: "note.txt", state: &editor.DiffState{Old: editor.NewBuffer(before), New: editor.NewBuffer(after)}}
	b.diff = d
	d.state.Revert(0)
	b.restore(task, d)
	pump(t, main, func() bool { return strings.Contains(logOf(task), "you reverted a change in note.txt") }, time.Minute)
	if task.Changes.Files != 0 {
		t.Fatalf("changes %+v", task.Changes)
	}
	b.reply(task, "Does the file note.txt exist right now? Check, then answer with exactly yes or no.")
	pump(t, main, settledOf(task), 5*time.Minute)
	if got := strings.ToLower(strings.Trim(strings.TrimSpace(task.Result), ".")); got != "no" {
		t.Errorf("the agent says %q", task.Result)
	}
}

// The agents past Claude Code, each on its own default model: a question
// through the board's MCP server whose answer resumes the conversation,
// edits committed, and a message while it works, which joins the turn for
// an agent that takes one and waits for the next turn otherwise.
func TestEvalAgents(t *testing.T) {
	for _, agent := range []string{"grok", "pi", "kimi", "opencode"} {
		t.Run(agent, func(t *testing.T) {
			if _, err := exec.LookPath(agentBins[agent]); err != nil {
				t.Skip("no " + agent + " on PATH")
			}
			repo := testRepo(t)
			b, main, w := evalBoard(t, "", t.TempDir())
			add := func(prompt string) *Task {
				b.dir, b.prompt, b.agent, b.model = repo, prompt, agent, ""
				b.add(true)
				return b.Tasks[len(b.Tasks)-1]
			}
			task := add("Use the ask_user tool of the kanban MCP server to ask me which color I want, red or blue, then end your turn. " +
				"Once I answer, write my answer, one word, to color.txt.")
			pump(t, main, settledOf(task), 5*time.Minute)
			t.Logf("log:\n%s", logOf(task))
			if task.Question == nil || len(w.notices) != 1 || task.SessionRef == "" {
				t.Fatalf("%s %q: question %+v, notices %+v", task.Status, task.Err, task.Question, w.notices)
			}
			if !strings.Contains(logOf(task), "▸ kanban/ask_user") {
				t.Errorf("the question's tool call is not logged as kanban/ask_user")
			}
			session := task.SessionRef
			b.reply(task, "blue")
			pump(t, main, settledOf(task), 5*time.Minute)
			t.Logf("log:\n%s", logOf(task))
			if got := read(filepath.Join(task.WS.Dir(), "color.txt")); task.Status != Review || got != "blue" || task.SessionRef != session {
				t.Errorf("%s %q: color.txt %q, session %s → %s", task.Status, task.Err, got, session, task.SessionRef)
			}
			if task.Usage.total() == 0 || task.Changes.Files != 1 {
				t.Errorf("usage %+v, changes %+v", task.Usage, task.Changes)
			}

			live := add("Run `sleep 3` with your shell tool, three separate times, one call each. Then create done.txt containing done. Reply with one short sentence.")
			pump(t, main, func() bool { return strings.Contains(logOf(live), "▸ Bash sleep") }, 5*time.Minute)
			said := b.say(live, "Also create extra.txt containing extra.", "")
			pump(t, main, settledOf(live), 5*time.Minute)
			t.Logf("log:\n%s", logOf(live))
			dir := live.WS.Dir()
			if live.Status != Review || read(filepath.Join(dir, "done.txt")) != "done" || read(filepath.Join(dir, "extra.txt")) != "extra" {
				t.Errorf("%s %q", live.Status, live.Err)
			}
			if liveAgents[agent] && (said != saidLive || live.Runs != 1) || !liveAgents[agent] && live.Runs != 2 || len(live.Asides) != 0 {
				t.Errorf("said %q, %d turns, asides %v", said, live.Runs, live.Asides)
			}
		})
	}
}

// A Grok Build session run in a terminal shows on the board, read-only:
// what it did, its tokens, and, once it is done, that it waits for you.
func TestEvalOutsideGrok(t *testing.T) {
	if _, err := exec.LookPath("grok"); err != nil {
		t.Skip("no grok on PATH")
	}
	dir := t.TempDir()
	cmd := exec.Command("grok", "-p", "Create hello.txt containing exactly hi. Then reply with one short sentence.", "--permission-mode", "bypassPermissions")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	t.Logf("terminal grok: %v\n%s", err, out)
	if read(filepath.Join(dir, "hello.txt")) != "hi" {
		t.Fatal("grok did not run")
	}
	b, _, _ := evalBoard(t, "", t.TempDir())
	b.setSessions(newWatcher().scan(time.Now()))
	real, _ := filepath.EvalSymlinks(dir)
	var s *Session
	for _, x := range b.sessions {
		if x.Agent == "grok" && (x.Dir == dir || x.Dir == real) {
			s = x
		}
	}
	if s == nil {
		t.Fatalf("not watched: %d sessions", len(b.sessions))
	}
	t.Logf("%s · %s · %d tokens\n%s", s.Title, s.Model, s.Tokens, strings.Join(s.Recent, "\n"))
	if s.Working || s.Tokens == 0 || s.Title == "" || !strings.Contains(strings.Join(s.Recent, "\n"), "▸ Write hello.txt") || b.sessionStatus(s) != Review {
		t.Errorf("%+v, status %s", s, b.sessionStatus(s))
	}
}
