package main

// Getting your attention. A task whose turn ends where it waits for you —
// a plan to confirm, a question, finished work, an error — is unread until
// you look at it. The Dock badge and the menu bar icon count them, a
// desktop notification names each while the window is in the background,
// and Cmd+J opens the next.

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/egoist/mygo"
)

// notice is a desktop notification about a task.
type notice struct {
	ID                    int
	Title, Subtitle, Body string
}

// noticeID names a task's notification, so a newer one replaces it and a
// click finds the task.
func noticeID(id int) string { return "task-" + strconv.Itoa(id) }

func taskOfNotice(id string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "task-"))
	return n, err == nil && strings.HasPrefix(id, "task-")
}

// isFocused reports whether you are looking at the board: its window is the
// key window. Without a window (tests) you are.
func (b *board) isFocused() bool { return b.focused == nil || b.focused() }

// settled is called when a task's turn has ended. Unless another turn of it
// goes on, it now waits for you: unread, unless you are looking at it, and
// announced while the board is in the background.
func (b *board) settled(t *Task) {
	if started(t) {
		return
	}
	if b.selected == t && b.isFocused() {
		return
	}
	t.Unread = true
	if !b.isFocused() && b.notify != nil {
		b.notify(noticeFor(t))
	}
}

// noticeFor says what a task waits for.
func noticeFor(t *Task) notice {
	n := notice{ID: t.ID, Title: fmt.Sprintf("#%d %s", t.ID, t.Title), Subtitle: t.Agent}
	switch {
	case t.Question != nil:
		n.Body = "Asks: " + t.Question.Text
	case t.Status == Confirm:
		n.Body = "The plan is ready for you to confirm."
	case t.Status == Failed:
		n.Body = "Failed: " + t.Err
	default:
		n.Body = "Done: " + cmp.Or(firstLine(t.Result), "the turn finished.")
	}
	n.Body = clip(n.Body, 240)
	return n
}

// unread lists the tasks waiting for you, by number.
func (b *board) unread() []*Task {
	var ts []*Task
	for _, t := range b.Tasks {
		if t.Unread {
			ts = append(ts, t)
		}
	}
	slices.SortFunc(ts, func(x, y *Task) int { return x.ID - y.ID })
	return ts
}

// look runs on every frame: the task you are looking at is read, and the
// Dock badge shows how many are not.
func (b *board) look() {
	if t := b.selected; t != nil && t.Unread && b.isFocused() {
		t.Unread = false
		b.save()
	}
	if n := len(b.unread()); n != b.badgeShown {
		b.badgeShown = n
		if b.badge != nil {
			b.badge(n)
		}
	}
	b.refreshTray()
}

// refreshTray sets the menu bar icon's count and menu again when what they
// show changed. It runs on every frame and every save, as the window may
// draw none while it is hidden.
func (b *board) refreshTray() {
	if b.tray == nil {
		return
	}
	key := fmt.Sprint(len(b.runs))
	for _, t := range b.unread() {
		key += fmt.Sprintf("|%d %s", t.ID, t.Title)
	}
	if key != b.trayShown {
		b.trayShown = key
		b.tray()
	}
}

// trayMenu is the menu bar icon's: the tasks waiting for you, to open one,
// and a new task. show brings the window up.
func (b *board) trayMenu(show func()) []*mygo.MenuItem {
	do := func(f func()) func(*mygo.MenuItem, *mygo.Window) {
		return func(*mygo.MenuItem, *mygo.Window) {
			b.update(f)
			show()
		}
	}
	ts := b.unread()
	head := fmt.Sprintf("%d waiting for you", len(ts))
	if len(ts) == 0 {
		head = "Nothing waiting for you"
	}
	items := []*mygo.MenuItem{{Label: head, Disabled: true}}
	for _, t := range ts[:min(len(ts), 9)] {
		items = append(items, &mygo.MenuItem{Label: fmt.Sprintf("#%d  %s", t.ID, clip(t.Title, 48)), Click: do(func() { b.open(t) })})
	}
	if n := len(b.runs); n > 0 {
		items = append(items, &mygo.MenuItem{Label: fmt.Sprintf("%d running", n), Disabled: true})
	}
	return append(items, mygo.Separator(),
		&mygo.MenuItem{Label: "New Task…", Click: do(func() { b.composing = true })},
		&mygo.MenuItem{Label: "Open Agent Kanban", Click: do(func() {})},
		mygo.Separator(),
		&mygo.MenuItem{Role: mygo.RoleQuit})
}

// jumpNext opens the next task waiting for you, after the one open.
func (b *board) jumpNext() {
	ts := b.unread()
	if len(ts) == 0 {
		return
	}
	next := ts[0]
	if b.selected != nil {
		for _, t := range ts {
			if t.ID > b.selected.ID {
				next = t
				break
			}
		}
	}
	b.open(next)
}

// open shows a task in the detail pane.
func (b *board) open(t *Task) {
	b.selected, b.selectedSession, b.tab = t, "", ""
}

// openNotice opens the task a clicked notification is about.
func (b *board) openNotice(id string) {
	n, ok := taskOfNotice(id)
	if !ok {
		return
	}
	for _, t := range b.Tasks {
		if t.ID == n {
			b.open(t)
		}
	}
}
