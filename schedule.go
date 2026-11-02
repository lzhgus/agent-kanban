package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// A Schedule files and starts a task on a timetable, as a check of the
// dependencies each morning. Each run is a task of its own, on its own
// branch.
type Schedule struct {
	ID                        int
	Title, Prompt, Agent, Dir string
	Model                     string
	PlanFirst, Lead           bool
	Every                     string // one of repeatChoices, past "Once"
	At                        string // "15:04"; only its minute for every hour
	Weekday                   time.Weekday
	Next                      time.Time
	Last                      int // the task it filed last
}

const once = "Once"

var repeatChoices = []string{once, "Every hour", "Every day", "Weekdays", "Every week"}

// parseAt reads "9:00" or "09:00".
func parseAt(s string) (h, m int, ok bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	return t.Hour(), t.Minute(), err == nil
}

// next is the first time after after the schedule runs.
func (s *Schedule) next(after time.Time) time.Time {
	h, m, _ := parseAt(s.At)
	y, mo, d := after.Date()
	if s.Every == "Every hour" {
		t := time.Date(y, mo, d, after.Hour(), m, 0, 0, after.Location())
		if !t.After(after) {
			t = t.Add(time.Hour)
		}
		return t
	}
	for i := 0; ; i++ { // at most 8 days
		t := time.Date(y, mo, d+i, h, m, 0, 0, after.Location())
		wd := t.Weekday()
		switch {
		case !t.After(after):
		case s.Every == "Weekdays" && (wd == time.Saturday || wd == time.Sunday):
		case s.Every == "Every week" && wd != s.Weekday:
		default:
			return t
		}
	}
}

// rule says when the schedule runs.
func (s *Schedule) rule() string {
	h, m, _ := parseAt(s.At)
	at := fmt.Sprintf("%02d:%02d", h, m)
	switch s.Every {
	case "Every hour":
		return fmt.Sprintf("Every hour at :%02d", m)
	case "Every week":
		return "Every " + s.Weekday.String() + " at " + at
	}
	return s.Every + " at " + at
}

// schedule makes the form's task a schedule rather than a task.
func (b *board) schedule(now time.Time) {
	prompt := strings.TrimSpace(b.prompt)
	if _, _, ok := parseAt(b.at); prompt == "" || !ok || b.repeat == once {
		return
	}
	b.NextScheduleID++
	s := &Schedule{ID: b.NextScheduleID, Title: strings.TrimSpace(b.title), Prompt: prompt, Agent: b.agent, Model: strings.TrimSpace(b.model),
		Dir: expandHome(strings.TrimSpace(b.dir)), PlanFirst: b.PlanFirst, Lead: b.lead, Every: b.repeat, At: b.at, Weekday: now.Weekday()}
	if s.Title == "" {
		s.Title = firstLine(prompt)
	}
	s.Next = s.next(now)
	b.Schedules = append(b.Schedules, s)
	b.LastDir = s.Dir
	b.title, b.prompt, b.lead, b.repeat = "", "", false, once
	b.save()
}

// fireDue runs the schedules whose time has come, each once however many
// runs it missed while the app was closed. One whose last task still runs
// skips its turn.
func (b *board) fireDue(now time.Time) {
	fired := false
	for _, s := range b.Schedules {
		if s.Next.After(now) {
			continue
		}
		s.Next, fired = s.next(now), true
		if last := b.task(s.Last); last != nil && last.Status == Running {
			continue
		}
		b.fire(s)
	}
	if fired {
		b.save()
	}
}

// fire files the schedule's task and starts it.
func (b *board) fire(s *Schedule) *Task {
	b.NextID++
	t := &Task{ID: b.NextID, Title: s.Title, Prompt: s.Prompt, Agent: s.Agent, Model: s.Model, Dir: s.Dir, Status: Todo,
		PlanFirst: s.PlanFirst, Lead: s.Lead, Schedule: s.ID}
	b.Tasks = append(b.Tasks, t)
	s.Last = t.ID
	b.start(t)
	return t
}

func (b *board) unschedule(s *Schedule) {
	b.Schedules = slices.DeleteFunc(b.Schedules, func(x *Schedule) bool { return x == s })
	b.save()
}
