package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
)

// planFake plays an agent that misbehaves while planning: it writes a file
// and commits it, which the board must undo. Building, it writes a file.
func planFake(t *testing.T, got *[]Task) driver {
	return func(ctx context.Context, task Task, log emit) (result, error) {
		*got = append(*got, task)
		if task.readOnly {
			os.WriteFile(filepath.Join(task.Dir, "STRAY.md"), []byte("no"), 0o644)
			exec.Command("git", "-C", task.Dir, "-c", "user.name=a", "-c", "user.email=a@a", "-c", "commit.gpgsign=false", "commit", "-qam", "stray").Run()
			os.WriteFile(filepath.Join(task.Dir, "app", "main.go"), []byte("edited while planning"), 0o644)
			return result{Text: "## Plan v" + string(rune('0'+len(*got))) + "\n\n1. Add NOTES.md", SessionRef: "s1"}, nil
		}
		os.WriteFile(filepath.Join(task.Dir, "NOTES.md"), []byte("built\n"), 0o644)
		return result{Text: "built it", SessionRef: "s1"}, nil
	}
}

func TestPlanFirst(t *testing.T) {
	var got []Task
	saved := drivers
	drivers = map[string]driver{"copilot": planFake(t, &got)}
	defer func() { drivers = saved }()
	repo := testRepo(t)
	queue := make(chan func(), 64)
	b := newBoard("")
	b.worktrees = t.TempDir()
	b.update = func(f func()) { queue <- f }
	drain := func(task *Task) {
		for task.Status == Running {
			(<-queue)()
		}
	}
	b.dir, b.prompt, b.title, b.agent, b.PlanFirst = repo, "Write notes", "Notes", "copilot", true
	b.add(true)
	task := b.Tasks[0]
	drain(task)

	// The plan: read-only, its stray edits and commit undone, waiting for you.
	if !got[0].readOnly || !strings.HasSuffix(got[0].Prompt, planNote) {
		t.Errorf("the planning turn ran %+v", got[0])
	}
	if task.Status != Confirm || task.Stage != "plan" || len(task.Plans) != 1 || task.Changes.Files != 0 {
		t.Fatalf("after planning: %+v", task)
	}
	if out := gitT(t, task.WS.Worktree, "status", "--porcelain"); out != "" || gitT(t, task.WS.Worktree, "rev-parse", "HEAD") != task.WS.BaseSHA {
		t.Errorf("the planning turn's edits stayed: %q", out)
	}
	if !strings.Contains(strings.Join(task.Log, "\n"), "were put back") {
		t.Errorf("log %q", task.Log)
	}

	// A reply revises the plan, still read-only.
	b.reply(task, "Also a TODO list")
	drain(task)
	if r := got[1]; !r.resume || !r.readOnly || r.Prompt != "Also a TODO list"+reviseNote {
		t.Errorf("the revision ran %+v", r)
	}
	if task.Status != Confirm || len(task.Plans) != 2 {
		t.Fatalf("after revising: %+v", task)
	}

	// Confirming builds it in the same conversation, with the note typed.
	task.draft = "keep it short"
	b.move(task, Running) // dragged on: confirmed
	drain(task)
	if r := got[2]; !r.resume || r.readOnly || r.Prompt != implementNote+"\n\nA note on it: keep it short" {
		t.Errorf("the build ran %+v", r)
	}
	if task.Status != Review || task.Stage != "build" || task.Changes != (Changes{Files: 1, Added: 1}) {
		t.Errorf("after building: %+v", task)
	}
	if strings.Count(strings.Join(task.Log, "\n"), "── plan confirmed") != 1 {
		t.Errorf("log %q", task.Log)
	}

	// Start over plans again, on a new branch.
	b.start(task)
	drain(task)
	if task.Status != Confirm || len(task.Plans) != 1 || task.WS.Branch != "kanban/1-notes-2" {
		t.Errorf("start over: %+v %+v", task, task.WS)
	}
}

// A plan whose conversation was lost is built in a new one, from the plan.
func TestConfirmWithoutSession(t *testing.T) {
	var got []Task
	saved := drivers
	drivers = map[string]driver{"claude": func(ctx context.Context, task Task, log emit) (result, error) {
		got = append(got, task)
		return result{Text: "done"}, nil
	}}
	defer func() { drivers = saved }()
	queue := make(chan func(), 16)
	b := newBoard("")
	b.update = func(f func()) { queue <- f }
	task := &Task{ID: 1, Title: "x", Prompt: "Do x", Agent: "claude", Dir: t.TempDir(), Status: Confirm, Runs: 1, Plans: []string{"1. x"}}
	b.Tasks = []*Task{task}
	b.confirm(task)
	for task.Status == Running {
		(<-queue)()
	}
	if r := got[0]; r.resume || r.readOnly || !strings.Contains(r.Prompt, "The approved plan:\n\n1. x") || !strings.HasPrefix(r.Prompt, "Do x") {
		t.Errorf("ran %+v", r)
	}
	if task.Status != Review {
		t.Errorf("status %q", task.Status)
	}
}

// Stopping a plan puts the task back where it was.
func TestStopPlanning(t *testing.T) {
	saved := drivers
	drivers = map[string]driver{"claude": func(ctx context.Context, task Task, _ emit) (result, error) {
		<-ctx.Done()
		return result{}, errors.New("signal: killed")
	}}
	defer func() { drivers = saved }()
	queue := make(chan func(), 16)
	b := newBoard("")
	b.update = func(f func()) { queue <- f }
	b.dir, b.prompt, b.PlanFirst = t.TempDir(), "x", true
	b.add(true)
	task := b.Tasks[0]
	b.stop(task)
	for task.Status == Running {
		(<-queue)()
	}
	if task.Status != Todo || len(task.Plans) != 0 {
		t.Errorf("after stopping the plan: %+v", task)
	}
	if !strings.Contains(strings.Join(task.Log, "\n"), "planning outside git") {
		t.Errorf("no warning about planning in place: %q", task.Log)
	}
}

// At Confirm the detail pane opens on the plan, and its button builds it.
func TestConfirmView(t *testing.T) {
	var got []Task
	saved := drivers
	drivers = map[string]driver{"claude": func(ctx context.Context, task Task, log emit) (result, error) {
		got = append(got, task)
		return result{Text: "done", SessionRef: task.SessionRef}, nil
	}}
	defer func() { drivers = saved }()
	queue := make(chan func(), 16)
	b := newBoard("")
	b.update = func(f func()) { queue <- f }
	task := &Task{ID: 1, Title: "Notes", Prompt: "x", Agent: "claude", Dir: t.TempDir(), Status: Confirm, Runs: 1,
		SessionRef: "s", Plans: []string{"old", "Add a **NOTES** file"}}
	b.Tasks, b.selected = []*Task{task}, task
	tt := ui.NewTester(b.view, 1440, 880)
	tt.Frame()
	if !tt.HasText("Plan · v2") || !tt.HasText("NOTES") || !tt.HasText("Plan ready") {
		t.Fatalf("the plan is not shown: %q", tt.Texts())
	}
	if err := tt.Click("Confirm plan"); err != nil {
		t.Fatal(err)
	}
	for task.Status == Running {
		(<-queue)()
	}
	if len(got) != 1 || got[0].readOnly || !got[0].resume || task.Status != Review {
		t.Errorf("confirm ran %+v; status %q", got, task.Status)
	}
}
