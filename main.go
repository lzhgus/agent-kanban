// Agent Kanban: a board whose cards are tasks run by the coding agents
// installed on this machine — Claude Code, Antigravity and GitHub Copilot.
// A card runs in its working directory, streams its log, lands in Review
// with its token usage, and can be handed off to another agent to review
// and continue the work.
//
//	go run .
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

type Status string

const (
	Todo    Status = "To Do"
	Running Status = "Running"
	Review  Status = "Review"
	Done    Status = "Done"
	Failed  Status = "Failed"
)

var columns = []Status{Todo, Running, Review, Done, Failed}

type Task struct {
	ID       int
	Title    string
	Prompt   string
	Agent    string
	Model    string // empty: the CLI's default
	Dir      string
	Status   Status
	From     int // the task this one was handed off from
	Log      []string
	Result   string // the agent's final answer
	Err      string
	Usage    Usage
	Runs     int // turns run, the first and every reply
	Duration time.Duration
	// The agent's conversation, resumed by each reply: a Claude Code session
	// id, an Antigravity conversation id or a Copilot session id.
	SessionRef string
	// Claude Code sessions of the task, which the watcher leaves to it.
	SessionIDs []string
	// The task's worktree and branch; nil when its folder is not in git and
	// the agent works there in place.
	WS      *Workspace
	Changes Changes // its branch against its base, after the last turn
	Merged  bool

	resume  bool   // this turn continues SessionRef (the drivers' copy only)
	draft   string // the reply being written in the detail panel
	merging bool
}

// newUUID returns a random version 4 UUID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

const logCap = 2000 // ponytail: lines kept per task; the board file grows with it

func (t *Task) addLog(line string) {
	t.Log = append(t.Log, line)
	if n := len(t.Log) - logCap; n > 0 {
		t.Log = slices.Delete(t.Log, 0, n)
	}
}

type board struct {
	Tasks   []*Task
	NextID  int
	LastDir string
	// Watched sessions dragged to a column, by key: the drag holds until the
	// session goes on.
	Placed map[string]placement

	path            string
	worktrees       string // where tasks' worktrees go
	selected        *Task
	detailTab       int        // 0 activity, 1 changes
	diff            *diffPane  // the changes tab's files and diff
	sessions        []*Session // from the watcher, replaced on each scan
	selectedSession string     // its key
	filter          string     // the agent the board shows; "" for all
	composing       bool       // the new-task composer is open
	runs            map[int]context.CancelFunc
	procs           sync.WaitGroup // drivers still running, for quitting
	update          func(func())   // runs a function on the main thread: Window.Update

	// the new-task form and the handoff picker
	title, prompt, agent, model, dir, handoffTo string
}

func newBoard(path string) *board {
	b := &board{path: path, runs: map[int]context.CancelFunc{}, agent: agentNames[0], handoffTo: agentNames[1],
		worktrees: filepath.Join(filepath.Dir(path), "worktrees")}
	if path == "" {
		b.worktrees = filepath.Join(os.TempDir(), "agent-kanban-worktrees")
	}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, b); err != nil {
			log.Printf("reading %s: %v", path, err)
		}
	}
	if b.Placed == nil {
		b.Placed = map[string]placement{}
	}
	for _, t := range b.Tasks {
		if t.Status == Running { // the app quit under it
			t.Status, t.Err = Failed, "interrupted: the app quit while it ran"
		}
	}
	b.dir = b.LastDir
	if b.dir == "" {
		b.dir, _ = os.UserHomeDir()
	}
	return b
}

// save writes the whole board. It runs on the main thread.
// ponytail: whole-file rewrite per change; fine for hundreds of cards.
func (b *board) save() {
	if b.path == "" {
		return
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(b.path), 0o755)
	}
	if err == nil {
		tmp := b.path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			err = os.Rename(tmp, b.path)
		}
	}
	if err != nil {
		log.Printf("saving the board: %v", err)
	}
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}

func (b *board) add(run bool) {
	prompt := strings.TrimSpace(b.prompt)
	if prompt == "" {
		return
	}
	title := strings.TrimSpace(b.title)
	if title == "" {
		title = firstLine(prompt)
	}
	b.NextID++
	t := &Task{ID: b.NextID, Title: title, Prompt: prompt, Agent: b.agent, Model: strings.TrimSpace(b.model), Dir: expandHome(strings.TrimSpace(b.dir)), Status: Todo}
	b.Tasks = append(b.Tasks, t)
	b.LastDir = t.Dir
	b.title, b.prompt = "", ""
	b.selected = t
	if run {
		b.start(t)
	} else {
		b.save()
	}
}

// start runs a task's prompt in a new conversation: its first turn, or a
// start over that leaves the old conversation behind.
func (b *board) start(t *Task) { b.turn(t, t.Prompt, false) }

// reply continues the task's conversation with a message: a revision turn.
func (b *board) reply(t *Task, message string) {
	if message = strings.TrimSpace(message); message == "" || t.SessionRef == "" {
		return
	}
	t.draft = ""
	b.turn(t, message, true)
}

// turn runs one turn of the task's agent on a goroutine of its own.
func (b *board) turn(t *Task, prompt string, resume bool) {
	if _, running := b.runs[t.ID]; running {
		return // one turn at a time per conversation
	}
	drive := drivers[t.Agent]
	if drive == nil {
		t.Status, t.Err = Failed, "unknown agent "+t.Agent
		b.save()
		return
	}
	if fi, err := os.Stat(t.Dir); err != nil || !fi.IsDir() {
		t.Status, t.Err = Failed, "working directory not found: "+t.Dir
		b.save()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.runs[t.ID] = cancel
	before := t.Status
	var drop *Workspace // a start over forks a new branch; the old one stays
	if !resume && t.Runs > 0 && t.WS != nil {
		if !b.shared(t) {
			drop = t.WS
		}
		t.WS, t.Changes, t.Merged = nil, Changes{}, false
	}
	var ws *Workspace
	if t.WS != nil {
		w := *t.WS
		ws = &w
	}
	t.Status, t.Err, t.Merged = Running, "", false // new work is not merged yet
	t.Runs++
	if !resume { // a new conversation
		t.SessionRef = ""
		if t.Agent == "claude" { // Claude takes the id we give it
			t.SessionRef = newUUID()
			t.SessionIDs = append(t.SessionIDs, t.SessionRef)
		}
	}
	if len(t.Log) > 0 {
		t.addLog("")
	}
	kind := "turn"
	if !resume && t.Runs > 1 {
		kind = "new conversation"
	}
	t.addLog(fmt.Sprintf("── %s %d · %s%s · %s", kind, t.Runs, t.Agent, modelSuffix(t.Model), time.Now().Format("15:04:05")))
	if resume {
		t.addLog("› " + prompt)
	}
	b.save()
	task, began := *t, time.Now() // the driver reads a copy, never the board
	task.Prompt, task.resume = prompt, resume
	b.procs.Add(1)
	go func() {
		defer b.procs.Done()
		log := func(line string) { b.update(func() { t.addLog(line) }) }
		if drop != nil {
			drop.remove()
		}
		ws, err := b.workspace(ws, task, log)
		if err != nil {
			b.update(func() { b.finish(t, result{}, err, time.Since(began), before, resume, turnGit{}) })
			return
		}
		if ws != nil {
			task.Dir = ws.Dir()
		}
		r, err := drive(ctx, task, log)
		if ctx.Err() != nil {
			err = ctx.Err() // a stopped agent reports its kill ("signal: killed"), not why
		}
		var g turnGit
		if ws != nil { // every turn ends in a commit, stopped and failed ones too
			g.changes, g.err = ws.checkpoint(task.Agent, commitMessage(task, task.Runs))
			g.ok = true
		}
		b.update(func() { b.finish(t, r, err, time.Since(began), before, resume, g) })
	}()
}

// workspace readies the turn's worktree: the task's own, brought back if it
// was deleted, or a new one for its first turn. It runs off the main thread.
func (b *board) workspace(ws *Workspace, task Task, log emit) (*Workspace, error) {
	if ws != nil {
		return ws, ws.ensure()
	}
	ws, note, err := openWorkspace(task.Dir, b.worktrees, task.ID, task.Title)
	if note != "" {
		log("note: " + note)
	}
	if err != nil || ws == nil {
		return nil, err
	}
	if err := os.MkdirAll(ws.Dir(), 0o755); err != nil { // an untracked folder is not checked out
		return nil, err
	}
	log("── branch " + ws.Branch + " in " + ws.Worktree)
	w := *ws
	b.update(func() {
		for _, t := range b.Tasks {
			if t.ID == task.ID {
				t.WS = &w
			}
		}
	})
	return ws, nil
}

// turnGit is what a turn's checkpoint found.
type turnGit struct {
	ok      bool
	changes Changes
	err     error
}

func commitMessage(t Task, turn int) string {
	subject := firstLine(t.Prompt)
	if len(subject) > 64 {
		subject = strings.TrimSpace(subject[:64]) + "…"
	}
	return fmt.Sprintf("%s: %s\n\nTurn %d of task #%d (%s).", t.Agent, subject, turn, t.ID, t.Title)
}

// shared reports whether another task works in t's worktree, as a hand-off
// does in the one it came from.
func (b *board) shared(t *Task) bool {
	for _, o := range b.Tasks {
		if o != t && o.WS != nil && t.WS != nil && o.WS.Worktree == t.WS.Worktree {
			return true
		}
	}
	return false
}

// merge merges the task's branch into its base in your checkout, and marks
// the task done.
func (b *board) merge(t *Task) {
	if t.WS == nil || t.merging || t.Status == Running {
		return
	}
	t.merging = true
	ws, title := *t.WS, t.Title
	go func() {
		err := ws.merge(title)
		b.update(func() {
			t.merging = false
			if err != nil {
				t.Err = "merge: " + err.Error()
				t.addLog("✗ " + t.Err)
			} else {
				t.Status, t.Err, t.Merged = Done, "", true
				t.addLog("── merged " + ws.Branch + " into " + ws.Base)
			}
			b.save()
		})
	}()
}

func modelSuffix(m string) string {
	if m == "" {
		return ""
	}
	return " (" + m + ")"
}

// finish records a turn's outcome. A stopped turn rests where the task was
// before it: To Do for a first turn, else Review, the conversation kept.
func (b *board) finish(t *Task, r result, err error, took time.Duration, before Status, resumed bool, g turnGit) {
	stopped := errors.Is(err, context.Canceled)
	if cancel := b.runs[t.ID]; cancel != nil {
		cancel()
		delete(b.runs, t.ID)
	}
	if g.ok {
		if g.err != nil {
			t.addLog("✗ commit: " + g.err.Error())
		} else {
			t.Changes = g.changes
		}
	}
	t.Usage.add(r.Usage)
	t.Duration += took
	if r.Text != "" {
		t.Result = r.Text
	}
	switch {
	case r.SessionRef != "":
		t.SessionRef = r.SessionRef
	case !resumed: // the agent never reported its conversation: nothing to continue
		t.SessionRef = ""
	}
	switch {
	case stopped:
		t.Status, t.Err = Review, ""
		if before == Todo {
			t.Status = Todo
		}
		t.addLog("── stopped")
	case err != nil:
		t.Status, t.Err = Failed, err.Error()
	case r.Err != "":
		t.Status, t.Err = Failed, r.Err
	default:
		t.Status = Review
	}
	if t.Err != "" {
		t.addLog("✗ " + t.Err)
	}
	b.save()
}

func (b *board) stop(t *Task) {
	if cancel := b.runs[t.ID]; cancel != nil {
		cancel()
	}
}

// stopAll ends every run as the app quits, waiting a moment for the agents'
// process groups to be killed.
func (b *board) stopAll() {
	for _, cancel := range b.runs {
		cancel()
	}
	done := make(chan struct{})
	go func() { b.procs.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

// handoff is how agents collaborate: another agent picks up the task in the
// same directory with the previous agent's report, to review and continue.
func (b *board) handoff(from *Task, agent string) {
	b.NextID++
	report := from.Result
	if report == "" {
		report = "(it left no report)"
	}
	t := &Task{
		ID:     b.NextID,
		Title:  "↪ " + strings.TrimPrefix(from.Title, "↪ "),
		Agent:  agent,
		Dir:    from.Dir,
		Status: Todo,
		From:   from.ID,
		Prompt: fmt.Sprintf("%s\n\n---\nAnother coding agent (%s) already worked on this task in this directory. Its report:\n\n%s\n\n"+
			"Review its work: inspect the changes%s, fix anything wrong, finish anything left undone, and report what you changed.",
			from.Prompt, from.Agent, report, changesHint(from.WS)),
	}
	if from.WS != nil { // the same branch: the reviewer works on what it reviews
		ws := *from.WS
		t.WS, t.Changes = &ws, from.Changes
	}
	b.Tasks = append(b.Tasks, t)
	b.selected = t
	b.start(t)
}

func (b *board) move(t *Task, to Status) {
	if t.Status == to || t.Status == Running {
		return
	}
	if to == Running {
		b.start(t)
		return
	}
	t.Status = to
	b.save()
}

// setSessions takes a scan from the watcher, leaving out the board's own runs.
func (b *board) setSessions(all []*Session) {
	ours := map[string]bool{}
	for _, t := range b.Tasks {
		for _, id := range t.SessionIDs {
			ours[id] = true
		}
	}
	b.sessions = slices.DeleteFunc(all, func(s *Session) bool { return s.Agent == "claude" && ours[s.ID] })
}

type placement struct {
	Status Status
	At     int64 // the session's last update when it was dragged, Unix ms
}

// sessionStatus places a watched session: at work in Running; then in
// Review, waiting for you, until it has been idle for idleDone, then Done.
// A drag to Review or Done holds until the session goes on.
func (b *board) sessionStatus(s *Session) Status {
	switch pl, ok := b.Placed[s.key()]; {
	case s.Working:
		return Running
	case ok && pl.At >= s.Updated.UnixMilli():
		return pl.Status
	case s.Failed != "":
		return Failed
	case time.Since(s.Updated) > idleDone:
		return Done
	}
	return Review
}

// moveSession drags a watched session to Review or Done.
func (b *board) moveSession(s *Session, to Status) {
	if to != Review && to != Done {
		return // the board cannot run or stop what it did not start
	}
	b.Placed[s.key()] = placement{Status: to, At: s.Updated.UnixMilli()}
	b.save()
}

func (b *board) findSession(key string) *Session {
	for _, s := range b.sessions {
		if s.key() == key {
			return s
		}
	}
	return nil
}

func changesHint(ws *Workspace) string {
	if ws == nil {
		return " in the working directory"
	}
	return fmt.Sprintf(" on this branch (%s, forked from %s: `git diff %s` shows them)", ws.Branch, ws.BaseSHA[:min(12, len(ws.BaseSHA))], ws.BaseSHA)
}

// remove deletes a task and its worktree; its branch, and the work on it,
// stay in your repository.
func (b *board) remove(t *Task) {
	b.stop(t)
	if t.WS != nil && !b.shared(t) {
		go t.WS.remove() // ponytail: a stopping turn may still commit there; git refuses then, and the worktree stays
	}
	b.Tasks = slices.DeleteFunc(b.Tasks, func(v *Task) bool { return v == t })
	if b.selected == t {
		b.selected = nil
	}
	b.save()
}

func main() {
	dir, err := os.UserConfigDir()
	if err != nil {
		log.Fatal(err)
	}
	b := newBoard(filepath.Join(dir, "agent-kanban", "board.json"))
	mygo.App.WhenReady(func() {
		win := mygo.NewWindow(mygo.WindowOptions{
			Title: "Agent Kanban", Width: 1440, Height: 880, MinWidth: 1000, MinHeight: 560,
			// No title bar: the traffic lights over a sidebar of
			// the desktop's material (macOS; elsewhere the view paints it).
			TitleBarStyle:  mygo.TitleBarHiddenInset,
			TitleBarHeight: titleBarHeight,
			Vibrancy:       mygo.VibrancySidebar,
			Content:        ui.View(b.view),
		})
		b.update = win.Update
		go func() { // watch the agents run outside the board
			w := newWatcher()
			for {
				sessions := w.scan(time.Now())
				win.Update(func() { b.setSessions(sessions) })
				time.Sleep(scanEvery)
			}
		}()
	})
	mygo.App.OnQuit(b.stopAll)
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
