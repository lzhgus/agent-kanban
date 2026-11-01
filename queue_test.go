package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// gated plays agents that run until the test lets them finish, by prompt.
type gated struct {
	gates map[string]chan struct{}
	mu    sync.Mutex // agents run at once
	ran   []string
}

func (g *gated) driver(ctx context.Context, task Task, log emit) (result, error) {
	g.mu.Lock()
	g.ran = append(g.ran, task.Prompt)
	g.mu.Unlock()
	if gate := g.gates[task.Prompt]; gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return result{}, ctx.Err()
		}
	}
	text := "done"
	if strings.Contains(task.Prompt, "VERDICT") { // a reviewer
		text = "Fine.\nVERDICT: APPROVE"
	}
	return result{Text: text, SessionRef: task.SessionRef}, nil
}

func (g *gated) install(t *testing.T) {
	saved := drivers
	drivers = map[string]driver{"claude": g.driver, "antigravity": g.driver, "copilot": g.driver}
	t.Cleanup(func() { drivers = saved })
}

func addTask(b *board, dir, prompt string) *Task {
	b.dir, b.prompt, b.agent = dir, prompt, "claude"
	b.add(true)
	return b.Tasks[len(b.Tasks)-1]
}

func TestQueue(t *testing.T) {
	g := &gated{gates: map[string]chan struct{}{"one": make(chan struct{}), "two": make(chan struct{}), "three": make(chan struct{})}}
	g.install(t)
	b, drain := boardWith(t)
	b.MaxRunning = 1
	dir := t.TempDir()
	one, two, three := addTask(b, dir, "one"), addTask(b, dir, "two"), addTask(b, dir, "three")
	if one.Status != Running || two.Status != Queued || three.Status != Queued || len(b.queue) != 2 || b.queue[0] != two {
		t.Fatalf("statuses %s %s %s, queue %d", one.Status, two.Status, three.Status, len(b.queue))
	}
	if !strings.Contains(strings.Join(two.Log, "\n"), "── queued: 1 agents are running") {
		t.Errorf("log %q", two.Log)
	}

	// A slot freed goes to the oldest turn waiting.
	close(g.gates["one"])
	drain(one)
	if one.Status != Review || two.Status != Running || three.Status != Queued {
		t.Fatalf("after one: %s %s %s", one.Status, two.Status, three.Status)
	}

	// Cancel takes a turn off the queue: back where it was.
	b.stop(three)
	if three.Status != Todo || len(b.queue) != 0 || three.pending != nil {
		t.Errorf("after cancelling: %s, queue %d", three.Status, len(b.queue))
	}

	// Raising the limit starts what waits at once.
	b.reply(one, "more")
	if one.Status != Queued {
		t.Fatalf("the reply did not queue: %s", one.Status)
	}
	b.setMaxRunning(2)
	if one.Status != Running || len(b.queue) != 0 {
		t.Errorf("after raising the limit: %s, queue %d", one.Status, len(b.queue))
	}
	close(g.gates["two"])
	drain(two)
	drain(one)
	g.mu.Lock()
	defer g.mu.Unlock()
	slices.Sort(g.ran) // two and more ran at once: in either order
	if strings.Join(g.ran, ",") != "more,one,two" {
		t.Errorf("ran %q", g.ran)
	}
}

// A review loop waits in the queue as any turn does, and goes on.
func TestQueueReviewLoop(t *testing.T) {
	g := &gated{gates: map[string]chan struct{}{"busy": make(chan struct{})}}
	g.install(t)
	b, drain := boardWith(t)
	b.MaxRunning = 1
	dir := t.TempDir()
	a := addTask(b, dir, "build it")
	drain(a)
	busy := addTask(b, dir, "busy")
	b.review(a, "copilot", 2)
	if a.Status != Queued || a.rev == nil {
		t.Fatalf("the review did not wait: %s, loop %v", a.Status, a.rev)
	}
	close(g.gates["busy"])
	drain(busy)
	drain(a)
	if a.Status != Review || a.rev != nil || !strings.Contains(strings.Join(a.Log, "\n"), "copilot approved (round 1)") {
		t.Errorf("after the queued review: %s, %q", a.Status, a.Log)
	}
}

// A queue does not outlive the app: its tasks go back where they were.
func TestQueuedOnRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.json")
	data, _ := json.Marshal(map[string]any{"Tasks": []*Task{{ID: 1, Status: Queued, QueuedFrom: Review}}})
	os.WriteFile(path, data, 0o600)
	b := newBoard(path)
	if task := b.Tasks[0]; task.Status != Review || !strings.Contains(strings.Join(task.Log, "\n"), "dropped") {
		t.Errorf("after a restart: %+v", task)
	}
}
