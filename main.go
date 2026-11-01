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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
)

type Status string

const (
	Todo    Status = "To Do"
	Running Status = "Running"
	Queued  Status = "Queued"  // waits for a free slot; shown in the Running column
	Confirm Status = "Confirm" // a plan waits for you; shown in the Review column
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
	// Plan first: a read-only turn writes a plan, which waits at Confirm
	// for you before the agent builds it.
	PlanFirst bool
	Plans     []string // the plan's versions, the last current
	Stage     string   // "plan" or "build": what its last turn did
	// What other agents spent on it, reviewing: by agent.
	Others map[string]Usage
	// Where a queued task was before its turn queued, to go back to.
	QueuedFrom Status
	// The question its agent asked you (ask_user), until you answer.
	Question *Question

	resume   bool   // this turn continues SessionRef (the drivers' copy only)
	readOnly bool   // this turn may read but not write (the drivers' copy only)
	draft    string // the reply being written in the detail panel
	merging  bool
	rev      *reviewLoop // the AI review going on, if one is
	pending  *queuedTurn // the turn waiting in the queue
	mcpURL   string      // the board's MCP server for this turn (the drivers' copy only)
	doing    string      // the running turn's kind, for its label
}

// Question is what an agent asked you: an answer is your next reply.
type Question struct {
	Text    string
	Options []string
	Turn    int // the turn that asked it
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

// spent is everything the task cost: its agent's turns and its reviews.
func (t *Task) spent() Usage {
	u := t.Usage
	for _, o := range t.Others {
		u.add(o)
	}
	return u
}

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
	// The new-task form's Plan first, kept for the next task.
	PlanFirst bool
	// MaxRunning is how many turns run at once; more wait in the queue.
	MaxRunning int

	path            string
	worktrees       string // where tasks' worktrees go
	selected        *Task
	tab             string // the detail pane's tab chosen; "" until one is
	md              markdown.Options
	diff            *diffPane         // the changes tab's files and diff
	bridge          *bridge           // the MCP server agents reach the board through; nil without one
	quotas          map[string]*Quota // each agent's plan windows, as last read
	quotaKick       chan struct{}     // a turn ended: read the quotas again soon
	sessions        []*Session        // from the watcher, replaced on each scan
	selectedSession string            // its key
	filter          string            // the agent the board shows; "" for all
	composing       bool              // the new-task composer is open
	runs            map[int]context.CancelFunc
	queue           []*Task        // tasks whose turn waits for a slot, first come first
	procs           sync.WaitGroup // drivers still running, for quitting
	update          func(func())   // runs a function on the main thread: Window.Update

	// the new-task form, the handoff picker and the review row
	title, prompt, agent, model, dir string
	reviewer, rounds                 string
	reviewerFor                      *Task
}

func newBoard(path string) *board {
	b := &board{path: path, runs: map[int]context.CancelFunc{}, agent: agentNames[0], rounds: roundChoices[0], quotaKick: make(chan struct{}, 1),
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
		switch t.Status {
		case Running: // the app quit under it
			t.Status, t.Err = Failed, "interrupted: the app quit while it ran"
		case Queued: // the turn waiting is gone with the app
			t.Status = t.QueuedFrom
			t.addLog("── the queued turn was dropped: the app quit")
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
	t := &Task{ID: b.NextID, Title: title, Prompt: prompt, Agent: b.agent, Model: strings.TrimSpace(b.model), Dir: expandHome(strings.TrimSpace(b.dir)), Status: Todo, PlanFirst: b.PlanFirst}
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

// What the board tells an agent about planning and building.
const (
	planNote = "\n\n---\nPlan first. Do not create, edit or delete any files, and do not commit: read what you need, " +
		"then reply with a plan in Markdown: the approach, the files you will change and how, how you will verify the change, " +
		"the risks, and any questions you need answered first. Your reply is the plan; it is reviewed before you implement it."
	reviseNote    = "\n\n---\nRevise the plan with this. Still change no files; reply with the whole revised plan."
	implementNote = "The plan is approved: implement it now, then verify the change and report what you did."
)

// turnOpts says what kind of turn to run.
type turnOpts struct {
	resume bool   // continue the task's conversation
	fresh  bool   // start over: a new conversation, and a new branch
	plan   bool   // a read-only planning turn, which ends at Confirm
	said   string // your words, for the log
	// reviewer runs a read-only review turn by that agent, in a conversation
	// of its own, instead of a turn of the task's agent.
	reviewer string
	revision bool // the task's agent answers a review: the loop goes on after it
}

// start runs a task's prompt in a new conversation: its first turn, or a
// start over that leaves the old conversation and branch behind. A task
// that plans first plans again.
func (b *board) start(t *Task) {
	prompt := t.Prompt
	if t.PlanFirst {
		prompt += planNote
	}
	b.turn(t, prompt, turnOpts{fresh: t.Runs > 0, plan: t.PlanFirst})
}

// reply continues the task's conversation with a message: a revision of
// the plan while it waits at Confirm, else of the work.
func (b *board) reply(t *Task, message string) {
	if message = strings.TrimSpace(message); message == "" || t.SessionRef == "" {
		return
	}
	t.draft = ""
	plan := t.Status == Confirm || (t.Status == Failed && t.Stage == "plan")
	prompt := message
	if plan {
		prompt += reviseNote
	}
	b.turn(t, prompt, turnOpts{resume: true, plan: plan, said: message})
}

// confirm approves the plan: the agent builds it, in the same conversation,
// with the reply being written as a note. A conversation that was lost
// starts anew with the plan in its prompt.
func (b *board) confirm(t *Task) {
	if t.Status != Confirm || len(t.Plans) == 0 {
		return
	}
	note := strings.TrimSpace(t.draft)
	t.draft = ""
	t.addLog("── plan confirmed")
	prompt := implementNote
	if note != "" {
		prompt += "\n\nA note on it: " + note
	}
	if t.SessionRef == "" {
		b.turn(t, t.Prompt+"\n\n---\nThe approved plan:\n\n"+t.Plans[len(t.Plans)-1]+"\n\n"+prompt, turnOpts{said: note})
		return
	}
	b.turn(t, prompt, turnOpts{resume: true, said: note})
}

const defaultMaxRunning = 3

func (b *board) maxRunning() int {
	if b.MaxRunning < 1 {
		return defaultMaxRunning
	}
	return b.MaxRunning
}

// queuedTurn is a turn waiting for a slot.
type queuedTurn struct {
	prompt string
	opts   turnOpts
}

// started reports whether a turn just asked for runs or waits to.
func started(t *Task) bool { return t.Status == Running || t.Status == Queued }

// dispatch starts queued turns while slots are free, oldest first.
func (b *board) dispatch() {
	for len(b.queue) > 0 && len(b.runs) < b.maxRunning() {
		t := b.queue[0]
		b.queue = b.queue[1:]
		q := t.pending
		t.Status, t.pending = t.QueuedFrom, nil
		b.turn(t, q.prompt, q.opts)
	}
}

// dequeue takes a task's turn off the queue: it goes back where it was.
func (b *board) dequeue(t *Task) {
	if t.Status != Queued {
		return
	}
	b.queue = slices.DeleteFunc(b.queue, func(v *Task) bool { return v == t })
	t.Status, t.pending, t.rev = t.QueuedFrom, nil, nil
	t.addLog("── taken off the queue")
	b.save()
}

func (b *board) setMaxRunning(n int) {
	b.MaxRunning = min(max(n, 1), 10)
	b.dispatch()
	b.save()
}

// turn runs one turn of the task's agent on a goroutine of its own, or,
// with every slot taken, queues it.
func (b *board) turn(t *Task, prompt string, o turnOpts) {
	if _, running := b.runs[t.ID]; running || t.Status == Queued {
		return // one turn at a time per conversation
	}
	agent := t.Agent
	if o.reviewer != "" {
		agent = o.reviewer
	}
	drive := drivers[agent]
	if drive == nil {
		t.Status, t.Err = Failed, "unknown agent "+agent
		b.save()
		return
	}
	if fi, err := os.Stat(t.Dir); err != nil || !fi.IsDir() {
		t.Status, t.Err = Failed, "working directory not found: "+t.Dir
		b.save()
		return
	}
	if len(b.runs) >= b.maxRunning() {
		t.QueuedFrom, t.Status, t.pending = t.Status, Queued, &queuedTurn{prompt, o}
		b.queue = append(b.queue, t)
		t.addLog(fmt.Sprintf("── queued: %d agents are running, the most at once", len(b.runs)))
		b.save()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.runs[t.ID] = cancel
	before := t.Status
	var drop *Workspace // a start over forks a new branch; the old one stays
	if o.fresh && t.WS != nil {
		if !b.shared(t) {
			drop = t.WS
		}
		t.WS, t.Changes, t.Merged = nil, Changes{}, false
	}
	if o.fresh {
		t.Plans = nil
	}
	var ws *Workspace
	if t.WS != nil {
		w := *t.WS
		ws = &w
	}
	t.Status, t.Err = Running, ""
	if o.reviewer == "" {
		t.Merged = false // new work is not merged yet
		t.Question = nil // a turn of its agent answers it, or moves past it
	}
	t.Runs++
	if !o.resume && o.reviewer == "" { // a new conversation
		t.SessionRef = ""
		if t.Agent == "claude" { // Claude takes the id we give it
			t.SessionRef = newUUID()
			t.SessionIDs = append(t.SessionIDs, t.SessionRef)
		}
	}
	if len(t.Log) > 0 {
		t.addLog("")
	}
	kind, model := fmt.Sprintf("turn %d", t.Runs), t.Model
	switch {
	case o.fresh:
		kind = "new conversation"
	case o.reviewer != "":
		kind, model = fmt.Sprintf("review %d/%d", t.rev.round, t.rev.rounds), ""
	case o.revision:
		kind = "revision"
	case o.plan:
		kind = "plan"
	}
	t.addLog(fmt.Sprintf("── %s · %s%s · %s", kind, agent, modelSuffix(model), time.Now().Format("15:04:05")))
	t.doing = kind
	if o.said != "" {
		t.addLog("› " + o.said)
	}
	readOnly := o.plan || o.reviewer != ""
	if readOnly && t.WS == nil && b.inPlace(t) {
		t.addLog("note: outside git, nothing undoes edits the agent makes anyway")
	}
	task, began := *t, time.Now() // the driver reads a copy, never the board
	task.Prompt, task.resume, task.readOnly = prompt, o.resume, readOnly
	if o.reviewer != "" { // the reviewer's own conversation, never the task's
		task.Agent, task.Model, task.SessionRef, task.resume = o.reviewer, "", "", false
		if o.reviewer == "claude" {
			task.SessionRef = newUUID()
			t.SessionIDs = append(t.SessionIDs, task.SessionRef) // the watcher leaves it to the task
		}
	}
	b.save()
	closeBridge := func() {}
	if b.bridge != nil {
		task.mcpURL, closeBridge = b.bridge.open(t, o.reviewer != "")
	}
	b.procs.Add(1)
	go func() {
		defer b.procs.Done()
		defer closeBridge()
		log := func(line string) { b.update(func() { t.addLog(line) }) }
		if drop != nil {
			drop.remove()
		}
		var err error
		if ws != nil || o.reviewer == "" { // a review uses the task's worktree, or its folder
			ws, err = b.workspace(ws, task, log)
		}
		if err != nil {
			b.update(func() { b.finish(t, result{}, err, time.Since(began), before, o, turnGit{}) })
			return
		}
		head := ""
		if ws != nil {
			task.Dir = ws.Dir()
			head, _ = git(ws.Worktree, "rev-parse", "HEAD")
		}
		r, err := drive(ctx, task, log)
		if ctx.Err() != nil {
			err = ctx.Err() // a stopped agent reports its kill ("signal: killed"), not why
		}
		var g turnGit
		switch {
		case ws != nil && readOnly: // a plan or a review changes nothing: what the agent left goes
			g.discarded, g.err = ws.discard(head)
		case ws != nil: // every turn ends in a commit, stopped and failed ones too
			g.changes, g.err = ws.checkpoint(task.Agent, commitMessage(task, task.Runs))
			g.ok = true
		}
		b.update(func() { b.finish(t, r, err, time.Since(began), before, o, g) })
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
	ok        bool // a checkpoint ran; changes is the branch's
	changes   Changes
	discarded int // files a planning turn changed, put back
	err       error
}

// inPlace reports whether the task works in its folder, outside git: true
// until its first turn says otherwise.
func (b *board) inPlace(t *Task) bool {
	_, err := git(t.Dir, "rev-parse", "--show-toplevel")
	return err != nil
}

func commitMessage(t Task, turn int) string {
	subject := firstLine(t.Prompt)
	if len(subject) > 64 {
		subject = strings.TrimSpace(subject[:64]) + "…"
	}
	return fmt.Sprintf("%s: %s\n\nTurn %d of task #%d (%s).", t.Agent, subject, turn, t.ID, t.Title)
}

// watchQuotas reads the agents' quotas now, every quotaEvery, and soon
// after a turn ends, though not more often than quotaAfter.
func (b *board) watchQuotas(update func(func())) {
	every := time.NewTicker(quotaEvery)
	defer every.Stop()
	var last time.Time
	for {
		if time.Since(last) >= quotaAfter {
			last = time.Now()
			q := probeQuotas()
			update(func() { b.quotas = q })
		}
		select {
		case <-every.C:
		case <-b.quotaKick:
			time.Sleep(10 * time.Second) // let the CLI record the turn first
		}
	}
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
	if t.WS == nil || t.merging || started(t) {
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
// before it; a plan rests at Confirm; work at Review.
func (b *board) finish(t *Task, r result, err error, took time.Duration, before Status, o turnOpts, g turnGit) {
	defer b.dispatch() // a slot is free: the next turn waiting takes it
	select {           // what it spent shows in the quotas
	case b.quotaKick <- struct{}{}:
	default:
	}
	stopped := errors.Is(err, context.Canceled)
	if cancel := b.runs[t.ID]; cancel != nil {
		cancel()
		delete(b.runs, t.ID)
	}
	switch {
	case g.err != nil && (o.plan || o.reviewer != ""):
		t.addLog("✗ undoing the read-only turn's edits: " + g.err.Error())
	case g.err != nil:
		t.addLog("✗ commit: " + g.err.Error())
	case g.ok:
		t.Changes = g.changes
	}
	if g.discarded > 0 {
		t.addLog(fmt.Sprintf("note: the read-only turn changed %d files; they were put back", g.discarded))
	}
	t.Duration += took
	if o.reviewer != "" {
		b.finishReview(t, r, err, before, o.reviewer)
		b.save()
		return
	}
	t.Usage.add(r.Usage)
	if r.Text != "" {
		t.Result = r.Text
	}
	switch {
	case r.SessionRef != "":
		t.SessionRef = r.SessionRef
	case !o.resume: // the agent never reported its conversation: nothing to continue
		t.SessionRef = ""
	}
	t.Stage = "build"
	if o.plan {
		t.Stage = "plan"
	}
	switch {
	case stopped:
		t.Status, t.Err, t.rev = before, "", nil // a stop ends a review loop too
		t.addLog("── stopped")
	case err != nil:
		t.Status, t.Err = Failed, err.Error()
	case r.Err != "":
		t.Status, t.Err = Failed, r.Err
	case o.plan && t.Question != nil && t.Question.Turn == t.Runs: // it asked before planning on
		t.Status = Confirm
	case o.plan && strings.TrimSpace(r.Text) == "":
		t.Status, t.Err = Failed, "the agent wrote no plan"
	case o.plan:
		t.Plans = append(t.Plans, strings.TrimSpace(r.Text))
		t.Status = Confirm
	default:
		t.Status = Review
	}
	if t.Err != "" {
		t.addLog("✗ " + t.Err)
	}
	if o.revision {
		b.afterRevision(t)
	}
	b.save()
}

func (b *board) stop(t *Task) {
	b.dequeue(t)
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
	if t.Status == to || started(t) {
		return
	}
	if to == Running {
		if t.Status == Confirm {
			b.confirm(t) // a plan dragged on is a plan confirmed
		} else {
			b.start(t)
		}
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
	if ln, err := net.Listen("tcp", "127.0.0.1:0"); err != nil {
		log.Printf("the agents' MCP server: %v; they run without it", err)
	} else {
		br := newBridge(b)
		br.base = "http://" + ln.Addr().String() + "/mcp/"
		go http.Serve(ln, br)
		b.bridge = br
	}
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
		go b.watchQuotas(win.Update)
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
