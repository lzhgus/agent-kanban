package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// watching records what the board tells you while its window is in the
// background.
type watching struct {
	notices []notice
	badges  []int
	focus   bool
}

func (w *watching) install(b *board) {
	b.notify = func(n notice) { w.notices = append(w.notices, n) }
	b.badge = func(n int) { w.badges = append(w.badges, n) }
	b.focused = func() bool { return w.focus }
}

// A turn that ends waiting for you marks its task unread, notifies while the
// board is in the background, and counts on the badge; looking reads it.
func TestAttention(t *testing.T) {
	g := &gated{gates: map[string]chan struct{}{"stop me": make(chan struct{})}}
	g.install(t)
	b, drain := boardWith(t)
	w := &watching{}
	w.install(b)
	dir := t.TempDir()

	done := addTask(b, dir, "build it")
	drain(done)
	b.PlanFirst = true
	plan := addTask(b, dir, "plan it")
	drain(plan)
	b.PlanFirst = false
	if !done.Unread || !plan.Unread || plan.Status != Confirm {
		t.Fatalf("unread %v %v, plan %s", done.Unread, plan.Unread, plan.Status)
	}
	if len(w.notices) != 2 || w.notices[0].Body != "Done: done" || w.notices[0].Title != "#1 build it" ||
		w.notices[1].Body != "The plan is ready for you to confirm." {
		t.Errorf("notices %+v", w.notices)
	}
	b.look()
	if last(w.badges) != 2 {
		t.Errorf("badge %v", w.badges)
	}

	stopped := addTask(b, dir, "stop me") // a turn you stop waits for nothing
	b.stop(stopped)
	drain(stopped)
	if stopped.Unread || len(w.notices) != 2 {
		t.Errorf("a stopped turn: unread %v, %d notices", stopped.Unread, len(w.notices))
	}

	b.jumpNext()
	if b.selected != done {
		t.Fatalf("Cmd+J opened %v", b.selected)
	}
	b.look() // in the background, opening is not reading
	if !done.Unread {
		t.Error("read while the board was in the background")
	}
	w.focus = true
	b.look()
	if done.Unread || last(w.badges) != 1 {
		t.Errorf("looked at: unread %v, badge %v", done.Unread, w.badges)
	}
	b.jumpNext()
	if b.selected != plan {
		t.Errorf("Cmd+J opened #%d", b.selected.ID)
	}
	b.look()

	b.reply(plan, "smaller") // you are looking at it: it ends read, silently
	drain(plan)
	if plan.Unread || len(w.notices) != 2 {
		t.Errorf("watched turn: unread %v, %d notices", plan.Unread, len(w.notices))
	}
}

// Cmd+J and the sidebar's count open the next task waiting for you; a card
// shows it waits.
func TestWaitingView(t *testing.T) {
	g := &gated{gates: map[string]chan struct{}{}}
	g.install(t)
	b, drain := boardWith(t)
	(&watching{}).install(b)
	one := addTask(b, t.TempDir(), "one")
	drain(one)
	b.selected = nil
	tt := ui.NewTester(b.view, 1440, 880)
	tt.Frame()
	if !tt.HasText("1 waiting for you") {
		t.Fatalf("no count: %q", tt.Texts())
	}
	tt.Key(ui.Cmd, ui.KeyJ)
	tt.Frame()
	if b.selected != one {
		t.Errorf("Cmd+J opened %v", b.selected)
	}
	b.openNotice(noticeID(one.ID))
	if b.selected != one {
		t.Error("the notification did not open its task")
	}
}

// resumed records the turns a driver ran.
type resumed struct {
	mu    sync.Mutex
	turns []Task
	gate  chan struct{}
}

func (r *resumed) driver(ctx context.Context, task Task, log emit) (result, error) {
	r.mu.Lock()
	r.turns = append(r.turns, task)
	gate := r.gate
	r.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return result{}, ctx.Err()
		}
	}
	return result{Text: "picked up", SessionRef: task.SessionRef}, nil
}

// The app quits while a turn runs: opened again, the board resumes that
// agent's conversation, once; an AI review goes back to its gate.
func TestResumeAfterQuit(t *testing.T) {
	r := &resumed{gate: make(chan struct{})}
	saved := drivers
	drivers = map[string]driver{"claude": r.driver, "copilot": r.driver}
	t.Cleanup(func() { drivers = saved })
	path := filepath.Join(t.TempDir(), "board.json")
	b := newBoard(path)
	b.worktrees = t.TempDir()
	quit := make(chan func(), 128) // the first app's: never run, it quit
	b.update = func(f func()) { quit <- f }
	dir := t.TempDir()
	work := addTask(b, dir, "work")
	b.PlanFirst = true
	planning := addTask(b, dir, "planning")
	b.PlanFirst = false
	if work.Status != Running || planning.Status != Running || work.SessionRef == "" {
		t.Fatalf("%s %s", work.Status, planning.Status)
	}
	b.save()

	// The app quits: its turns never finish. Add the other cases by hand.
	b.Tasks = append(b.Tasks,
		&Task{ID: 10, Title: "reviewing", Agent: "claude", Dir: dir, Status: Running, Doing: "review 1/2", SessionRef: "s10", Stage: "plan", Plans: []string{"p"}},
		&Task{ID: 11, Title: "resumed already", Agent: "claude", Dir: dir, Status: Running, Doing: resumedKind, SessionRef: "s11"},
		&Task{ID: 12, Title: "no conversation", Agent: "copilot", Dir: dir, Status: Running, Doing: "turn 1"})
	b.save()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) { // the first app's turns are under way
		r.mu.Lock()
		n := len(r.turns)
		r.mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
	}
	r.mu.Lock()
	r.turns, r.gate = nil, nil // and stay blocked
	r.mu.Unlock()

	queue = make(chan func(), 128)
	b2 := newBoard(path)
	b2.worktrees = b.worktrees
	b2.update = func(f func()) { queue <- f }
	byID := map[int]*Task{}
	for _, task := range b2.Tasks {
		byID[task.ID] = task
	}
	if s := byID[10].Status; s != Confirm {
		t.Errorf("the review: %s", s)
	}
	if s := byID[11]; s.Status != Failed || !strings.Contains(s.Err, "interrupted") {
		t.Errorf("resumed twice: %s %q", s.Status, s.Err)
	}
	if s := byID[12].Status; s != Failed {
		t.Errorf("no conversation: %s", s)
	}
	if len(b2.interrupted) != 2 {
		t.Fatalf("%d to resume", len(b2.interrupted))
	}
	b2.resumeInterrupted()
	for _, id := range []int{work.ID, planning.ID} {
		task := byID[id]
		for task.Status == Running {
			(<-queue)()
		}
	}
	if len(r.turns) != 2 {
		t.Fatalf("%d turns", len(r.turns))
	}
	for _, turn := range r.turns {
		if !turn.resume || !strings.HasPrefix(turn.Prompt, resumeNote) {
			t.Errorf("#%d not resumed: %v %q", turn.ID, turn.resume, turn.Prompt)
		}
		if turn.ID == planning.ID && (!turn.readOnly || !strings.Contains(turn.Prompt, resumePlanNote)) {
			t.Errorf("the plan resumed writable: %q", turn.Prompt)
		}
		if turn.SessionRef != byID[turn.ID].SessionRef {
			t.Errorf("#%d another conversation", turn.ID)
		}
	}
	if w, p := byID[work.ID], byID[planning.ID]; w.Status != Review || p.Status != Confirm || w.Doing != resumedKind {
		t.Errorf("after resuming: %s %s %q", w.Status, p.Status, w.Doing)
	}
}

func last(xs []int) int {
	if len(xs) == 0 {
		return -1
	}
	return xs[len(xs)-1]
}
