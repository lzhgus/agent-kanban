package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

func TestScheduleNext(t *testing.T) {
	at := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, time.UTC) } // Nov 2, 2026 is a Monday
	for _, c := range []struct {
		s     Schedule
		after time.Time
		want  time.Time
		rule  string
	}{
		{Schedule{Every: "Every day", At: "9:00"}, at(2, 8, 0), at(2, 9, 0), "Every day at 09:00"},
		{Schedule{Every: "Every day", At: "09:00"}, at(2, 9, 0), at(3, 9, 0), "Every day at 09:00"},
		{Schedule{Every: "Weekdays", At: "18:30"}, at(6, 19, 0), at(9, 18, 30), "Weekdays at 18:30"}, // Friday evening: Monday
		{Schedule{Every: "Every week", At: "09:00", Weekday: time.Wednesday}, at(4, 10, 0), at(11, 9, 0), "Every Wednesday at 09:00"},
		{Schedule{Every: "Every hour", At: "09:15"}, at(2, 8, 20), at(2, 9, 15), "Every hour at :15"},
		{Schedule{Every: "Every hour", At: "09:15"}, at(2, 8, 10), at(2, 8, 15), "Every hour at :15"},
		{Schedule{Every: "Every day", At: "23:30"}, at(30, 23, 45), time.Date(2026, 12, 1, 23, 30, 0, 0, time.UTC), "Every day at 23:30"},
	} {
		if got := c.s.next(c.after); !got.Equal(c.want) || c.s.rule() != c.rule {
			t.Errorf("%s after %v: %v, %q", c.s.Every, c.after, got, c.s.rule())
		}
	}
}

// A schedule files a task of its own each time it comes due, once however
// many runs it missed; it skips a run while its last task still runs. Its
// row in the sidebar runs it now or deletes it.
func TestSchedules(t *testing.T) {
	g := &gated{gates: map[string]chan struct{}{"check deps": make(chan struct{})}}
	g.install(t)
	b, drain := boardWith(t)
	dir := t.TempDir()
	monday := time.Date(2026, 11, 2, 8, 0, 0, 0, time.Local)
	b.dir, b.prompt, b.agent, b.title, b.repeat, b.at = dir, "check deps", "claude", "Deps", "Every day", "25:00"
	b.schedule(monday)
	if len(b.Schedules) != 0 {
		t.Fatal("scheduled at 25:00")
	}
	b.at = "09:00"
	b.schedule(monday)
	s := b.Schedules[0]
	if len(b.Tasks) != 0 || s.Title != "Deps" || s.Dir != dir || !s.Next.Equal(monday.Add(time.Hour)) || b.repeat != once || b.prompt != "" {
		t.Fatalf("%+v", s)
	}
	b.fireDue(monday.Add(30 * time.Minute))
	if len(b.Tasks) != 0 {
		t.Fatal("fired early")
	}
	b.fireDue(monday.Add(3 * 24 * time.Hour)) // three missed: one run
	if len(b.Tasks) != 1 || b.Tasks[0].Status != Running || b.Tasks[0].Schedule != s.ID || !s.Next.Equal(monday.AddDate(0, 0, 3).Add(time.Hour)) {
		t.Fatalf("tasks %d, next %v", len(b.Tasks), s.Next)
	}
	b.fireDue(s.Next) // the last still runs
	if len(b.Tasks) != 1 {
		t.Fatal("ran over its last run")
	}
	close(g.gates["check deps"])
	drain(b.Tasks[0])
	b.fireDue(s.Next)
	if len(b.Tasks) != 2 {
		t.Fatal("did not run again")
	}
	drain(b.Tasks[1])

	tt := ui.NewTester(b.view, 1440, 880)
	tt.Frame()
	if !tt.HasText("Next " + s.Next.Format("Mon Jan 2, 15:04")) {
		t.Fatalf("the sidebar shows %q", tt.Texts())
	}
	r, _ := tt.Find("Deps")
	tt.Move(r.X+r.W/2, r.Y+r.H/2)
	tt.Frame()
	if err := tt.Click("Run now"); err != nil {
		t.Fatal(err)
	}
	if len(b.Tasks) != 3 {
		t.Fatal("run now")
	}
	drain(b.Tasks[2])
	tt.Move(r.X+r.W/2, r.Y+r.H/2)
	tt.Frame()
	if err := tt.Click("Delete schedule"); err != nil {
		t.Fatal(err)
	}
	if len(b.Schedules) != 0 || len(b.Tasks) != 3 {
		t.Errorf("deleted: %d schedules, %d tasks", len(b.Schedules), len(b.Tasks))
	}
}

// The composer schedules a task rather than adding it when it repeats.
func TestComposeSchedule(t *testing.T) {
	b, _ := boardWith(t)
	b.composing, b.prompt, b.repeat, b.dir = true, "triage new issues", "Weekdays", t.TempDir()
	tt := ui.NewTester(b.view, 1440, 880)
	tt.Frame()
	if tt.HasText("Add & Run") {
		t.Error("a repeating task offers to run once")
	}
	if err := tt.Click("Schedule"); err != nil {
		t.Fatal(err)
	}
	if len(b.Schedules) != 1 || b.Schedules[0].Every != "Weekdays" || b.Schedules[0].At != "09:00" || len(b.Tasks) != 0 || b.composing {
		t.Errorf("%+v, %d tasks", b.Schedules, len(b.Tasks))
	}
}

// The menu bar icon lists the tasks waiting for you, and opens them.
func TestTrayMenu(t *testing.T) {
	b, _ := boardWith(t)
	var sets, shown int
	b.tray = func() { sets++ }
	labels := func() []string {
		var out []string
		for _, it := range b.trayMenu(func() { shown++ }) {
			out = append(out, it.Label)
		}
		return out
	}
	if got := labels(); got[0] != "Nothing waiting for you" || !slices.Contains(got, "New Task…") {
		t.Errorf("empty: %q", got)
	}
	b.Tasks = []*Task{{ID: 3, Title: "Fix login", Unread: true}, {ID: 4, Title: "Docs"}}
	b.look()
	b.look()
	if sets != 1 {
		t.Errorf("set %d times", sets)
	}
	items := b.trayMenu(func() { shown++ })
	if items[0].Label != "1 waiting for you" || items[1].Label != "#3  Fix login" {
		t.Fatalf("%q", labels())
	}
	items[1].Click(items[1], nil)
	(<-queue)()
	if b.selected != b.Tasks[0] || shown != 1 {
		t.Errorf("opened %v, shown %d", b.selected, shown)
	}
	i := slices.IndexFunc(items, func(it *mygo.MenuItem) bool { return it.Label == "New Task…" })
	items[i].Click(items[i], nil)
	(<-queue)()
	if !b.composing {
		t.Error("New Task… did not open the composer")
	}
}

// Revert on a change in the Changes tab puts its old lines back on the
// task's branch, as a commit of yours.
func TestRevertChange(t *testing.T) {
	s := &scripted{replies: map[string][]func(Task) (result, error){
		"claude": {func(task Task) (result, error) {
			os.WriteFile(filepath.Join(task.Dir, "main.go"), []byte("package main\n\nimport \"fmt\"\n\nfunc main() {}\n\nfunc extra() {}\n"), 0o644)
			os.WriteFile(filepath.Join(task.Dir, "new.go"), []byte("package main\n"), 0o644)
			return result{Text: "done"}, nil
		}},
	}}
	s.install(t)
	b, drain := boardWith(t)
	task := addTask(b, filepath.Join(testRepo(t), "app"), "edit")
	drain(task)
	b.open(task)
	b.tab = "Changes"
	tt := ui.NewTester(b.view, 1440, 880)
	frames := func(done func() bool) {
		for range 200 {
			tt.Frame()
			select {
			case f := <-queue:
				f()
			case <-time.After(10 * time.Millisecond):
			}
			if done() {
				return
			}
		}
		t.Fatal("timed out")
	}
	frames(func() bool {
		return b.diff != nil && b.diff.state != nil && b.diff.path == "app/main.go" && b.diff.state.HunkActions
	})
	d := b.diff
	if hs := d.state.Hunks(); len(hs) != 2 || !d.state.HunkActions {
		t.Fatalf("hunks %+v, actions %v", hs, d.state.HunkActions)
	}
	d.state.Revert(1) // func extra
	frames(func() bool {
		return strings.Contains(strings.Join(task.Log, "\n"), "you reverted a change in app/main.go")
	})
	if got, _ := os.ReadFile(filepath.Join(task.WS.Dir(), "main.go")); string(got) != "package main\n\nimport \"fmt\"\n\nfunc main() {}\n" {
		t.Errorf("main.go %q", got)
	}
	if log := gitT(t, task.WS.Worktree, "log", "-1", "--format=%an %s"); log != "You (Agent Kanban) Revert a change in app/main.go" {
		t.Errorf("commit %q", log)
	}
	if task.Changes.Files != 2 || task.Changes.Added != 3 {
		t.Errorf("changes %+v", task.Changes)
	}
	// An added file reverted whole goes.
	b.openFile(d, "app/new.go")
	frames(func() bool { return d.state != nil && d.path == "app/new.go" })
	d.state.Revert(0)
	frames(func() bool { return task.Changes.Files == 1 })
	if _, err := os.Stat(filepath.Join(task.WS.Dir(), "new.go")); err == nil {
		t.Error("new.go is still there")
	}
}
