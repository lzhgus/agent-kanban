package main

import (
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// onlyInstalled pretends these agents' CLIs are the ones on PATH.
func onlyInstalled(t *testing.T, agents ...string) {
	saved := installed
	installed = func(a string) bool {
		for _, x := range agents {
			if x == a {
				return true
			}
		}
		return false
	}
	t.Cleanup(func() { installed = saved })
}

func used(n float64) *Quota { return &Quota{Windows: []Window{{Label: "5h", Used: n}}, At: time.Now()} }

func TestPickAgent(t *testing.T) {
	onlyInstalled(t, "claude", "copilot")
	b := newBoard("")
	b.quotas = map[string]*Quota{"claude": used(80), "copilot": used(20), "antigravity": used(0)}
	if a := b.pickAgent(true); a != "copilot" {
		t.Errorf("the most left among the installed: %q", a)
	}
	b.Tasks = []*Task{{ID: 1, Agent: "copilot", Status: Running, picked: true}}
	if a := b.pickAgent(true); a != "claude" {
		t.Errorf("copilot is busy: %q", a)
	}
	if a := b.pickAgent(false); a != "copilot" {
		t.Errorf("by hand, busy does not count: %q", a)
	}
	b.quotas["claude"] = used(100)
	if a := b.pickAgent(true); a != "" {
		t.Errorf("claude is spent: %q", a)
	}
	b.quotas["claude"] = &Quota{Err: "not signed in"}
	if a := b.pickAgent(true); a != "" {
		t.Errorf("claude is signed out: %q", a)
	}
}

// With Auto on, To Do works itself off: oldest first, one task per agent,
// "any" to the free agent with the most left, within the slots.
func TestAutoPickup(t *testing.T) {
	onlyInstalled(t, "claude", "copilot")
	g := &gated{gates: map[string]chan struct{}{"A": make(chan struct{}), "B": make(chan struct{}), "C": make(chan struct{}), "D": make(chan struct{})}}
	g.install(t)
	b, drain := boardWith(t)
	b.quotas = map[string]*Quota{"claude": used(50), "copilot": used(10)}
	dir := t.TempDir()
	b.dir = dir
	for _, x := range []struct{ prompt, agent string }{{"A", anyAgent}, {"B", "claude"}, {"C", "claude"}, {"D", anyAgent}} {
		b.quickAdd(x.prompt, x.agent)
	}
	a, bt, c, d := b.Tasks[0], b.Tasks[1], b.Tasks[2], b.Tasks[3]
	if a.Status != Todo {
		t.Fatal("a task started with Auto off")
	}

	b.setAutoPickup(true)
	if a.Agent != "copilot" || a.Status != Running || bt.Status != Running || c.Status != Todo || d.Status != Todo || d.Agent != anyAgent {
		t.Fatalf("after Auto: A %s/%s B %s C %s D %s/%s", a.Agent, a.Status, bt.Status, c.Status, d.Agent, d.Status)
	}
	if !strings.Contains(strings.Join(a.Log, "\n"), "picked up by copilot (auto)") {
		t.Errorf("log %q", a.Log)
	}

	close(g.gates["B"]) // claude is free: C, the oldest it can take
	drain(bt)
	if c.Status != Running || c.Agent != "claude" || d.Status != Todo {
		t.Errorf("after B: C %s, D %s", c.Status, d.Status)
	}
	close(g.gates["A"]) // copilot is free: D
	drain(a)
	if d.Status != Running || d.Agent != "copilot" {
		t.Errorf("after A: D %s/%s", d.Agent, d.Status)
	}
	close(g.gates["C"])
	close(g.gates["D"])
	drain(c)
	drain(d)
	if a.picked || c.picked {
		t.Error("a finished task still holds its agent")
	}
}

// Auto takes free slots only; a task an agent filed plans first.
func TestAutoPickupLimits(t *testing.T) {
	onlyInstalled(t, "claude", "copilot")
	g := &gated{gates: map[string]chan struct{}{"one": make(chan struct{})}}
	g.install(t)
	b, drain := boardWith(t)
	b.MaxRunning = 1
	b.dir = t.TempDir()
	b.quickAdd("one", "claude")
	b.quickAdd("two", "copilot")
	b.Tasks[1].ByAgent = true
	b.setAutoPickup(true)
	one, two := b.Tasks[0], b.Tasks[1]
	if one.Status != Running || two.Status != Todo || len(b.queue) != 0 {
		t.Fatalf("one slot: %s %s, queue %d", one.Status, two.Status, len(b.queue))
	}
	close(g.gates["one"])
	drain(one)
	drain(two)
	if !two.PlanFirst || two.Status != Confirm || !strings.Contains(strings.Join(two.Log, "\n"), "filed by an agent") {
		t.Errorf("the agent's task: plan first %v, %s", two.PlanFirst, two.Status)
	}
}

// A task left to any agent and started by hand goes to whoever has the
// most left; with none installed, it fails saying so.
func TestStartAnyByHand(t *testing.T) {
	onlyInstalled(t, "copilot")
	g := &gated{gates: map[string]chan struct{}{}}
	g.install(t)
	b, drain := boardWith(t)
	b.dir = t.TempDir()
	b.quickAdd("x", anyAgent)
	task := b.Tasks[0]
	b.start(task)
	drain(task)
	if task.Agent != "copilot" || task.Status != Review {
		t.Errorf("%s %s", task.Agent, task.Status)
	}
	onlyInstalled(t)
	b.quickAdd("y", anyAgent)
	if b.start(b.Tasks[1]); b.Tasks[1].Status != Failed {
		t.Errorf("with no agent: %s", b.Tasks[1].Status)
	}
}

// A double click on To Do opens the quick add; Enter files the task.
func TestQuickAddView(t *testing.T) {
	onlyInstalled(t)
	b, _ := boardWith(t)
	b.dir = t.TempDir()
	tt := ui.NewTester(b.view, 1440, 880)
	tt.Frame()
	r, ok := tt.Find("To Do")
	if !ok {
		t.Fatal("no To Do column")
	}
	x, y := r.X+r.W/2, r.Y+300 // the column's empty space
	tt.ClickAt(x, y)
	tt.ClickAt(x, y)
	tt.Frame()
	if !b.quickAdding || !tt.HasText("Double-click to add a task") == tt.HasText("What should an agent do?") {
		t.Fatalf("the double click did not open the quick add: %v %q", b.quickAdding, tt.Texts())
	}
	b.quick = "Fix the flaky test\nIt fails one run in ten."
	tt.Frame()
	if err := tt.Click("Add"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if len(b.Tasks) != 1 || b.Tasks[0].Title != "Fix the flaky test" || b.Tasks[0].Agent != anyAgent || b.quickAdding {
		t.Errorf("filed %+v", b.Tasks)
	}
	if err := tt.Click("Auto pickup"); err != nil {
		t.Fatal(err)
	}
	if !b.AutoPickup {
		t.Error("the switch did not turn Auto on")
	}
}

// A column takes both what is dragged onto it: a task dragged to Running
// starts, a session dragged to Done is done.
func TestDragCards(t *testing.T) {
	onlyInstalled(t, "antigravity")
	g := &gated{gates: map[string]chan struct{}{"run me": make(chan struct{})}}
	g.install(t)
	b, drain := boardWith(t)
	b.dir = t.TempDir()
	b.quickAdd("run me", "antigravity")
	task := b.Tasks[0]
	sess := &Session{Agent: "codex", ID: "s1", Title: "outside", Updated: time.Now().Add(-time.Minute)}
	b.setSessions([]*Session{sess})
	tt := ui.NewTester(b.view, 1440, 880)
	tt.Frame()
	drag := func(label, column string) {
		from, ok1 := tt.Find(label)
		to, ok2 := tt.Find(column)
		if !ok1 || !ok2 {
			t.Fatalf("no %q or %q", label, column)
		}
		x, y, tx, ty := from.X+20, from.Y+20, to.X+20, to.Y+300 // into the column, under its header
		tt.Press(x, y)
		for k := float32(1); k <= 4; k++ {
			tt.Move(x+(tx-x)*k/4, y+(ty-y)*k/4)
		}
		tt.Release(tx, ty)
		tt.Frame()
	}
	drag("run me", "Running")
	if !started(task) {
		t.Errorf("the task dragged to Running: %s", task.Status)
	}
	drag("outside", "Done")
	if st := b.sessionStatus(sess); st != Done {
		t.Errorf("the session dragged to Done: %s", st)
	}
	close(g.gates["run me"])
	drain(task)
}

// A task that never ran moves to the folder chosen; the quick add files in
// the folder chosen there. Cards show they take a click.
func TestChooseFolder(t *testing.T) {
	onlyInstalled(t)
	saved := pickFolder
	pickFolder = func(string) string { return "/x/other" }
	t.Cleanup(func() { pickFolder = saved })
	b, _ := boardWith(t)
	b.dir = "/x/app"
	b.quickAdd("fix it", "claude")
	task := b.Tasks[0]
	b.selected = task
	tt := ui.NewTester(b.view, 1440, 880)
	tt.Frame()
	chosen := func() {
		t.Helper()
		select {
		case f := <-queue:
			f()
		case <-time.After(5 * time.Second):
			t.Fatal("no folder came back")
		}
		tt.Frame()
	}
	if err := tt.Click("Change folder"); err != nil {
		t.Fatal(err)
	}
	chosen()
	if task.Dir != "/x/other" || b.LastDir != "/x/other" {
		t.Errorf("the task's folder: %q", task.Dir)
	}

	b.selected, b.dir, b.quickAdding = nil, "/x/app", true
	tt.Frame()
	if err := tt.Click("Folder"); err != nil {
		t.Fatal(err)
	}
	chosen()
	b.quickAdd("another", "claude")
	if b.Tasks[1].Dir != "/x/other" {
		t.Errorf("the quick add's folder: %q", b.Tasks[1].Dir)
	}

	task.Runs = 1 // ran there: it stays
	b.setDir(task, "/x/third")
	if task.Dir != "/x/other" {
		t.Errorf("a task that ran moved: %q", task.Dir)
	}

	tt.Frame()
	r, ok := tt.Find("another")
	if !ok {
		t.Fatal("no card")
	}
	tt.Move(r.X+r.W/2, r.Y+r.H/2)
	tt.Frame()
	if tt.Cursor() != ui.CursorPointer {
		t.Errorf("cursor over a card: %v", tt.Cursor())
	}
}
