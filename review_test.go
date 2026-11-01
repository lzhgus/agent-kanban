package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestParseVerdict(t *testing.T) {
	for _, c := range []struct {
		in       string
		critique string
		approve  bool
	}{
		{"Looks right.\n\nVERDICT: APPROVE", "Looks right.", true},
		{"1. Fix the nil check\n**VERDICT: CHANGES**\n", "1. Fix the nil check", false},
		{"Verdict: approve — ship it", "", true},
		{"No verdict here", "No verdict here", false},
	} {
		critique, approve := parseVerdict(c.in)
		if critique != c.critique || approve != c.approve {
			t.Errorf("%q: %q %v, want %q %v", c.in, critique, approve, c.critique, c.approve)
		}
	}
}

// scripted plays agents: each call takes the next reply of that agent.
type scripted struct {
	calls   []Task
	replies map[string][]func(Task) (result, error)
}

func (s *scripted) driver(name string) driver {
	return func(ctx context.Context, task Task, log emit) (result, error) {
		s.calls = append(s.calls, task)
		next := s.replies[name][0]
		s.replies[name] = s.replies[name][1:]
		return next(task)
	}
}

func (s *scripted) install(t *testing.T) {
	saved := drivers
	drivers = map[string]driver{}
	for _, a := range agentNames {
		drivers[a] = s.driver(a)
	}
	t.Cleanup(func() { drivers = saved })
}

func says(text string) func(Task) (result, error) {
	return func(task Task) (result, error) {
		return result{Text: text, SessionRef: task.SessionRef, Usage: Usage{Input: 10, Output: 1}}, nil
	}
}

func writes(file, text string) func(Task) (result, error) {
	return func(task Task) (result, error) {
		os.WriteFile(filepath.Join(task.Dir, file), []byte(text), 0o644)
		return result{Text: "wrote " + file, SessionRef: task.SessionRef, Usage: Usage{Input: 100, Output: 10}}, nil
	}
}

var queue chan func() // boardWith's: the board's updates, run by the test

func boardWith(t *testing.T) (*board, func(*Task)) {
	queue = make(chan func(), 128)
	b := newBoard("")
	b.worktrees = t.TempDir()
	b.update = func(f func()) { queue <- f }
	return b, func(task *Task) {
		for task.Status == Running {
			(<-queue)()
		}
	}
}

func TestReviewLoop(t *testing.T) {
	s := &scripted{replies: map[string][]func(Task) (result, error){
		"claude": {writes("api.go", "v1\n"), writes("api.go", "v2 with nil check\n")},
		"copilot": {
			func(task Task) (result, error) { // a reviewer that edits anyway: undone
				os.WriteFile(filepath.Join(task.Dir, "STRAY"), []byte("x"), 0o644)
				return says("1. Missing nil check in api.go\nVERDICT: CHANGES")(task)
			},
			says("Fixed.\nVERDICT: APPROVE"),
		},
	}}
	s.install(t)
	b, drain := boardWith(t)
	b.dir, b.prompt, b.title, b.agent = testRepo(t), "Write the API", "API", "claude"
	b.add(true)
	task := b.Tasks[0]
	drain(task)
	base := task.WS.BaseSHA

	b.review(task, "copilot", 3)
	drain(task)

	if len(s.calls) != 4 {
		t.Fatalf("%d turns ran, want build, review, revision, review", len(s.calls))
	}
	r1, rev, r2 := s.calls[1], s.calls[2], s.calls[3]
	if r1.Agent != "copilot" || !r1.readOnly || r1.resume || !strings.Contains(r1.Prompt, "git diff "+base) || strings.Contains(r1.Prompt, "previous review") {
		t.Errorf("round 1 ran %+v", r1)
	}
	if rev.Agent != "claude" || rev.readOnly || !rev.resume || !strings.Contains(rev.Prompt, "copilot reviewed your changes:\n\n1. Missing nil check") || strings.Contains(rev.Prompt, "VERDICT") {
		t.Errorf("the revision ran %+v", rev)
	}
	if !strings.Contains(r2.Prompt, "Your previous review said:\n\n1. Missing nil check") {
		t.Errorf("round 2 did not carry the critique: %q", r2.Prompt)
	}
	if task.Status != Review || task.rev != nil || task.Stage != "build" {
		t.Errorf("after the loop: %+v", task)
	}
	log := strings.Join(task.Log, "\n")
	for _, want := range []string{"review 1/3 · copilot", "revision · claude", "review 2/3 · copilot", "copilot approved (round 2)", "were put back"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q:\n%s", want, log)
		}
	}
	if _, err := os.Stat(filepath.Join(task.WS.Worktree, "STRAY")); err == nil {
		t.Error("the reviewer's edit stayed")
	}
	if got, _ := os.ReadFile(filepath.Join(task.WS.Worktree, "api.go")); string(got) != "v2 with nil check\n" {
		t.Errorf("the revision's work: %q", got)
	}
	if task.Others["copilot"] != (Usage{Input: 20, Output: 2}) || task.Usage != (Usage{Input: 200, Output: 20}) {
		t.Errorf("usage: own %+v, others %+v", task.Usage, task.Others)
	}
}

// Out of rounds, the loop leaves the gate to you; a plan is reviewed as a plan.
func TestReviewPlanRounds(t *testing.T) {
	s := &scripted{replies: map[string][]func(Task) (result, error){
		"claude":      {says("## Plan\n1. x")},
		"antigravity": {says("Too vague.\nVERDICT: CHANGES")},
	}}
	s.install(t)
	b, drain := boardWith(t)
	b.dir, b.prompt, b.agent, b.PlanFirst = testRepo(t), "Do x", "claude", true
	b.add(true)
	task := b.Tasks[0]
	drain(task)
	s.replies["claude"] = append(s.replies["claude"], says("## Plan\n1. x, in detail"))
	b.review(task, "antigravity", 1)
	drain(task)
	if r := s.calls[1]; !strings.Contains(r.Prompt, "The plan:\n\n## Plan\n1. x") || !r.readOnly {
		t.Errorf("the plan review ran %+v", r)
	}
	if rev := s.calls[2]; !rev.readOnly || !strings.HasSuffix(rev.Prompt, reviseNote) {
		t.Errorf("the plan revision ran %+v", rev)
	}
	if task.Status != Confirm || len(task.Plans) != 2 || task.rev != nil || len(s.calls) != 3 {
		t.Errorf("after one round: %+v, %d calls", task, len(s.calls))
	}
	if !strings.Contains(strings.Join(task.Log, "\n"), "review done: 1 rounds") {
		t.Errorf("log %q", task.Log)
	}
}

// A stop ends the loop; a failed review returns to the gate.
func TestReviewStopAndFail(t *testing.T) {
	stopped := make(chan struct{})
	s := &scripted{replies: map[string][]func(Task) (result, error){
		"claude":  {says("built"), func(Task) (result, error) { <-stopped; return result{}, errors.New("signal: killed") }},
		"copilot": {says("Bad.\nVERDICT: CHANGES"), func(Task) (result, error) { return result{}, errors.New("copilot: not signed in") }},
	}}
	s.install(t)
	b, drain := boardWith(t)
	b.dir, b.prompt, b.agent = t.TempDir(), "x", "claude"
	b.add(true)
	task := b.Tasks[0]
	drain(task)

	b.review(task, "copilot", 3)
	for task.doing != "revision" { // the review asked for changes; the revision runs
		(<-queue)()
	}
	b.stop(task)
	close(stopped)
	drain(task)
	if task.Status != Review || task.rev != nil || len(s.calls) != 3 {
		t.Errorf("after stopping the revision: %+v, %d calls", task, len(s.calls))
	}

	b.review(task, "copilot", 2)
	drain(task)
	if task.Status != Review || task.rev != nil || !strings.Contains(strings.Join(task.Log, "\n"), "✗ review: copilot: not signed in") {
		t.Errorf("after a failed review: %+v", task)
	}
}

// The review row picks another agent, and its button starts the loop.
func TestReviewRow(t *testing.T) {
	s := &scripted{replies: map[string][]func(Task) (result, error){"antigravity": {says("OK\nVERDICT: APPROVE")}}}
	s.install(t)
	b, drain := boardWith(t)
	task := &Task{ID: 1, Title: "x", Prompt: "x", Agent: "claude", Dir: t.TempDir(), Status: Review, Runs: 1, SessionRef: "s"}
	b.Tasks, b.selected = []*Task{task}, task
	tt := ui.NewTester(b.view, 1440, 880)
	tt.Frame()
	if b.reviewer != "antigravity" {
		t.Errorf("the reviewer picked: %q", b.reviewer)
	}
	if err := tt.Click("Start review"); err != nil {
		t.Fatal(err)
	}
	drain(task)
	if len(s.calls) != 1 || s.calls[0].Agent != "antigravity" || !strings.Contains(strings.Join(task.Log, "\n"), "antigravity approved") {
		t.Errorf("the review ran %+v", s.calls)
	}
}
