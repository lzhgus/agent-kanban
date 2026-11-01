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
