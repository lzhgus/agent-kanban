package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ends is a driver that writes the file its prompt names and reports where
// it left the conversation: e1, e2, …
type ends struct{ turns []Task }

func (e *ends) driver(ctx context.Context, task Task, log emit) (result, error) {
	e.turns = append(e.turns, task)
	name := strings.Fields(task.Prompt)[0]
	os.WriteFile(filepath.Join(task.Dir, name), []byte(name), 0o644)
	return result{Text: "wrote " + name, SessionRef: task.SessionRef, EndRef: "e" + string(rune('0'+len(e.turns)))}, nil
}

// Undo takes the code back to before the last turn and resumes the
// conversation where the turn before left it; undoing the first turn
// leaves the task as if it never ran.
func TestUndoTurn(t *testing.T) {
	e := &ends{}
	saved := drivers
	drivers = map[string]driver{"claude": e.driver}
	t.Cleanup(func() { drivers = saved })
	b, drain := boardWith(t)
	task := addTask(b, testRepo(t), "a.txt")
	drain(task)
	b.reply(task, "b.txt")
	drain(task)
	dir := task.WS.Dir()
	if len(task.Marks) != 2 || !b.canUndo(task) || task.Changes.Files != 2 {
		t.Fatalf("marks %+v, changes %+v", task.Marks, task.Changes)
	}
	undo := func() {
		b.undo(task)
		for task.undoing {
			(<-queue)()
		}
	}
	undo()
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err == nil || task.Changes.Files != 1 || task.ResumeAt != "e1" || task.Status != Review {
		t.Fatalf("after undoing turn 2: changes %+v, resume at %q, %s", task.Changes, task.ResumeAt, task.Status)
	}
	b.reply(task, "c.txt")
	drain(task)
	last := e.turns[len(e.turns)-1]
	if last.ResumeAt != "e1" || !strings.Contains(strings.Join(claudeArgs(last), " "), "--resume-session-at e1") || task.ResumeAt != "" {
		t.Errorf("the next turn: resume at %q", last.ResumeAt)
	}
	undo() // turn 3
	undo() // turn 1: as if it never ran
	if task.Status != Todo || task.SessionRef != "" || task.Runs != 0 || task.Changes.Files != 0 || b.canUndo(task) {
		t.Errorf("after undoing it all: %s %q runs %d changes %+v", task.Status, task.SessionRef, task.Runs, task.Changes)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err == nil {
		t.Error("a.txt is still there")
	}
	b.start(task)
	drain(task)
	if task.Status != Review || task.Runs != 1 || task.WS.Dir() != dir {
		t.Errorf("run again: %s, %d runs, in %s", task.Status, task.Runs, task.WS.Dir())
	}
}
