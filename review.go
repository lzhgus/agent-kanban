package main

// AI review: at a gate, another agent reviews the plan or the changes,
// read-only, and ends with a verdict. Changes asked for go back to the
// task's agent, which revises in its own conversation and returns to the
// gate; the next round reviews again, until the reviewer approves or the
// rounds run out. The reviewer only advises: the gate stays yours.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func isStopped(err error) bool { return errors.Is(err, context.Canceled) }

// reviewLoop is a review going on over a task.
type reviewLoop struct {
	reviewer      string
	round, rounds int
	gate          Status // Confirm (the plan) or Review (the changes)
	critique      string // the last review, carried into the next
}

const maxRounds = 3

// review starts a review of the task at its gate: rounds rounds at most.
func (b *board) review(t *Task, reviewer string, rounds int) {
	if t.rev != nil || (t.Status != Confirm && t.Status != Review) || drivers[reviewer] == nil {
		return
	}
	if t.Status == Confirm && len(t.Plans) == 0 {
		return
	}
	t.rev = &reviewLoop{reviewer: reviewer, round: 1, rounds: min(max(rounds, 1), maxRounds), gate: t.Status}
	b.reviewTurn(t)
}

func (b *board) reviewTurn(t *Task) {
	b.turn(t, reviewPrompt(t, t.rev), turnOpts{reviewer: t.rev.reviewer})
	if !started(t) { // it could not start
		t.rev = nil
	}
}

func reviewPrompt(t *Task, l *reviewLoop) string {
	var p strings.Builder
	what := "changes"
	if l.gate == Confirm {
		what = "plan"
	}
	fmt.Fprintf(&p, "You are reviewing another coding agent's %s for a task. Do not create, edit or delete any files, and do not commit.\n\n", what)
	fmt.Fprintf(&p, "The task:\n\n%s\n\n", t.Prompt)
	if l.gate == Confirm {
		fmt.Fprintf(&p, "The plan:\n\n%s\n\n", t.Plans[len(t.Plans)-1])
	} else if t.WS != nil {
		fmt.Fprintf(&p, "The changes are on this branch, %s: run `git diff %s` to see them, and read the files around them.\n\n", t.WS.Branch, t.WS.BaseSHA)
	} else {
		p.WriteString("The changes are in this directory; look at what changed.\n\n")
	}
	if l.critique != "" {
		fmt.Fprintf(&p, "Your previous review said:\n\n%s\n\nThe agent has revised since: check whether each point was addressed.\n\n", l.critique)
	}
	p.WriteString("Review for correctness, missed requirements, risks, and simpler ways to do it. Be concrete: name files and lines. Skip style nits.\n")
	p.WriteString("End your reply with one line, exactly `VERDICT: APPROVE` if it is ready as it is, or `VERDICT: CHANGES` if it needs changes.")
	return p.String()
}

// parseVerdict splits a review into its critique and whether it approves.
// A review with no verdict asks for changes.
func parseVerdict(text string) (critique string, approve bool) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		v := strings.ToUpper(strings.Trim(strings.TrimSpace(lines[i]), "*`_ "))
		if rest, ok := strings.CutPrefix(v, "VERDICT:"); ok {
			critique = strings.TrimSpace(strings.Join(append(lines[:i:i], lines[i+1:]...), "\n"))
			return critique, strings.HasPrefix(strings.TrimSpace(rest), "APPROVE")
		}
	}
	return strings.TrimSpace(text), false
}

// finishReview takes a review turn's outcome: approved, the loop ends at
// the gate; changes asked for, the task's agent revises.
func (b *board) finishReview(t *Task, r result, err error, before Status, reviewer string) {
	l := t.rev
	if reviewer == t.Agent {
		t.Usage.add(r.Usage)
	} else {
		if t.Others == nil {
			t.Others = map[string]Usage{}
		}
		u := t.Others[reviewer]
		u.add(r.Usage)
		t.Others[reviewer] = u
	}
	t.Status, t.rev = before, nil // back at the gate unless the loop goes on
	critique, approve := parseVerdict(r.Text)
	switch {
	case isStopped(err):
		t.addLog("── review stopped")
	case err != nil || r.Err != "":
		msg := r.Err
		if err != nil {
			msg = err.Error()
		}
		t.addLog("✗ review: " + msg)
	case l == nil:
	case approve:
		t.addLog(fmt.Sprintf("── %s approved (round %d)", reviewer, l.round))
	case critique == "":
		t.addLog("✗ review: " + reviewer + " wrote no review")
	case t.SessionRef == "":
		t.addLog("note: " + t.Agent + "'s conversation is gone; reply to it with the review yourself")
	default:
		l.critique = critique
		t.rev = l
		b.revise(t, l)
	}
}

// revise hands the review to the task's agent, in its conversation.
func (b *board) revise(t *Task, l *reviewLoop) {
	what := "changes"
	if l.gate == Confirm {
		what = "plan"
	}
	prompt := fmt.Sprintf("%s reviewed your %s:\n\n%s\n\nAddress the points you agree with, and say which you disagree with and why.", l.reviewer, what, l.critique)
	if l.gate == Confirm {
		prompt += reviseNote
	} else {
		prompt += " Then report what you changed."
	}
	b.turn(t, prompt, turnOpts{resume: true, plan: l.gate == Confirm, revision: true})
	if !started(t) {
		t.rev = nil
	}
}

// afterRevision goes on to the next round, or ends the loop.
func (b *board) afterRevision(t *Task) {
	l := t.rev
	if l == nil {
		return
	}
	if t.Status != l.gate || l.round >= l.rounds {
		t.rev = nil
		if t.Status == l.gate {
			t.addLog(fmt.Sprintf("── review done: %d rounds, the last asked for changes; the gate is yours", l.rounds))
		}
		return
	}
	l.round++
	b.reviewTurn(t)
}
