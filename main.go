// Agent Kanban: a board whose cards are tasks run by the coding agents
// installed on this machine — Claude Code, Antigravity and GitHub Copilot.
// A card runs in its working directory, streams its log, lands in Review
// with its token usage, and can be handed off to another agent to review
// and continue the work.
//
//	go run .
package main

import (
	"cmp"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
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
	// The agents' conversations of the task, which the watcher leaves to it.
	SessionIDs []string
	// The task's worktree and branch; nil when its folder is not in git and
	// the agent works there in place.
	WS      *Workspace
	Changes Changes // its branch against its base, after the last turn
	Merged  bool
	PR      string // the URL of its branch's pull request, once you opened one
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
	// ByAgent: an agent filed it (create_todo); auto pickup plans it first.
	ByAgent bool
	// Unread: its turn ended where it waits for you, and you have not
	// looked at it since.
	Unread bool
	// Doing is the last turn's kind ("turn 2", "plan", "review 1/3", …):
	// its label while it runs, and after a restart, whether to resume it.
	Doing string
	// Asides are messages for its agent that wait for the turn running to
	// end: they are its next turn.
	Asides []string
	// Note is what its agent last said it is doing (report_status).
	Note string
	// ReadOnly: every turn may read but not write, as research does.
	ReadOnly bool
	// Sub: part of the task it came From, filed by that task's agent. It
	// works on a branch of that task's branch, and needs no plan confirmed.
	Sub bool
	// Lead: its agent splits the work into subtasks for other agents.
	Lead bool
	// InPlace: it works in its folder itself, never a worktree, as the
	// terminal session it took over did.
	InPlace bool
	// Marks are where each of its turns began and ended, the last last:
	// what undoing a turn takes the code and the conversation back to.
	Marks []TurnMark
	// ResumeAt: the next turn resumes the conversation at this point, as
	// turns after it were undone.
	ResumeAt string
	// Schedule is the schedule that filed it, if one did.
	Schedule int

	resume   bool          // this turn continues SessionRef (the drivers' copy only)
	readOnly bool          // this turn may read but not write (the drivers' copy only)
	draft    string        // the reply being written in the detail panel
	inbox    <-chan string // messages while the turn runs, for a live agent (the drivers' copy only)
	merging  bool
	undoing  bool
	pushing  bool
	rev      *reviewLoop // the AI review going on, if one is
	pending  *queuedTurn // the turn waiting in the queue
	mcpURL   string      // the board's MCP server for this turn (the drivers' copy only)
	picked   bool        // auto pickup started its turn: its agent is busy with it
}

// TurnMark is a turn's place in the task's history.
type TurnMark struct {
	Turn  int
	Head  string // the branch's commit before it
	End   string // where it left the agent's conversation
	First bool   // it began the conversation
	Plan  bool   // it wrote a version of the plan
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
	// AutoPickup: To Do tasks start by themselves as agents come free.
	AutoPickup bool
	// OutsideToken is the secret in the URL of the board's MCP server for
	// agents outside it, such as Claude Code in your terminal.
	OutsideToken string
	// Schedules file tasks on a timetable (schedule.go).
	Schedules      []*Schedule
	NextScheduleID int

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
	quickAdding     bool              // the To Do column's quick add is open
	quick           string            // its text
	quickAgent      string            // and its agent
	repeat          string            // the composer's Repeat: once, or a schedule's
	at              string            // and its time
	runs            map[int]context.CancelFunc
	inboxes         map[int]chan string // the running turns' messages, of the live agents
	queue           []*Task             // tasks whose turn waits for a slot, first come first
	procs           sync.WaitGroup      // drivers still running, for quitting
	writing         sync.Mutex          // reverted changes being written to a branch
	update          func(func())        // runs a function on the main thread: Window.Update
	interrupted     []*Task             // tasks whose turn the app quit under, to resume
	// Getting your attention (attention.go); nil without a window.
	notify     func(notice)
	focused    func() bool
	badge      func(int)
	badgeShown int
	tray       func()       // sets the menu bar icon's count and menu again
	trayShown  string       // what they show
	copyText   func(string) // to the clipboard
	outsideURL string       // the MCP server for agents outside the board; "" without one
	copied     time.Time    // when the terminal command was copied, for its label

	// the new-task form, the handoff picker and the review row
	title, prompt, agent, model, dir string
	lead                             bool
	reviewer, rounds                 string
	reviewerFor                      *Task
}

func newBoard(path string) *board {
	b := &board{path: path, runs: map[int]context.CancelFunc{}, inboxes: map[int]chan string{}, agent: agentNames[0], rounds: roundChoices[0], repeat: once, at: "09:00", quotaKick: make(chan struct{}, 1),
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
			b.quitUnder(t)
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
	b.refreshTray()
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
	t := b.file(title, prompt, b.agent, strings.TrimSpace(b.model))
	t.Lead = b.lead
	b.title, b.prompt, b.lead = "", "", false
	b.selected = t
	if run {
		b.start(t)
	} else {
		b.save()
		b.autopick()
	}
}

// file puts a task in To Do, in the folder of the form.
func (b *board) file(title, prompt, agent, model string) *Task {
	b.NextID++
	t := &Task{ID: b.NextID, Title: title, Prompt: prompt, Agent: agent, Model: model, Dir: expandHome(strings.TrimSpace(b.dir)), Status: Todo, PlanFirst: b.PlanFirst}
	b.Tasks = append(b.Tasks, t)
	b.LastDir = t.Dir
	return t
}

// pickFolder asks for a folder with the system's dialog, opening at start;
// "" when canceled. It blocks until then.
var pickFolder = func(start string) string {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Title: "Choose the folder the agent works in", DefaultPath: start,
		Directory: true, CreateDirectories: true, ButtonLabel: "Choose"})
	if err != nil || len(paths) == 0 {
		return ""
	}
	return paths[0]
}

// chooseFolder asks for a folder off the UI thread and gives it to set.
func (b *board) chooseFolder(start string, set func(dir string)) {
	go func() {
		if dir := pickFolder(expandHome(strings.TrimSpace(start))); dir != "" {
			b.update(func() { set(dir) })
		}
	}()
}

// setDir moves a task that never ran to another folder.
func (b *board) setDir(t *Task, dir string) {
	if t.Runs > 0 || started(t) {
		return
	}
	t.Dir, t.WS, b.LastDir = dir, nil, dir
	b.save()
}

// quickAdd files a task from the To Do column: its first line is its title.
func (b *board) quickAdd(text, agent string) {
	if text = strings.TrimSpace(text); text == "" {
		return
	}
	b.file(firstLine(text), text, agent, "")
	b.save()
	b.autopick()
}

// What the board tells an agent about planning and building.
const (
	planNote = "\n\n---\nPlan first. Do not create, edit or delete any files, and do not commit: read what you need, " +
		"then reply with a plan in Markdown: the approach, the files you will change and how, how you will verify the change, " +
		"the risks, and any questions you need answered first. Your reply is the plan; it is reviewed before you implement it."
	reviseNote    = "\n\n---\nRevise the plan with this. Still change no files; reply with the whole revised plan."
	implementNote = "The plan is approved: implement it now, then verify the change and report what you did."
	leadNote      = "\n\n---\nYou lead this task, with other agents. Split the work into parts that can go on in parallel, " +
		"and file each as a subtask with the kanban MCP server's create_todo tool (subtask: true; read_only: true for research that changes no files, " +
		"so its agent reports what it found). Each starts right away, on a branch of your branch's last commit, so commit what a subtask needs first. " +
		"Work on your own part meanwhile. Wait for each with wait_todo and read its report; use message_todo to steer one while it works, or to ask it for more after. " +
		"Merge each subtask's branch into yours with git merge, resolve conflicts, check that it all works together, and report what each agent did."
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
	revision bool   // the task's agent answers a review: the loop goes on after it
	kind     string // the turn's label, when not the usual
}

// start runs a task's prompt in a new conversation: its first turn, or a
// start over that leaves the old conversation and branch behind. A task
// that plans first plans again.
func (b *board) start(t *Task) {
	if t.Agent == anyAgent { // started by hand: whoever has the most of its plan left
		a := b.pickAgent(false)
		if a == "" {
			t.Status, t.Err = Failed, "no agent is installed and signed in to take it"
			b.save()
			return
		}
		t.Agent = a
		t.addLog("── given to " + a)
	}
	prompt := t.Prompt
	if t.Lead {
		prompt += leadNote
	}
	if t.PlanFirst {
		prompt += planNote
	}
	b.turn(t, prompt, turnOpts{fresh: t.Runs > 0, plan: t.PlanFirst})
}

// reply sends what you wrote in the detail panel.
func (b *board) reply(t *Task, message string) {
	if strings.TrimSpace(message) == "" {
		return
	}
	t.draft = ""
	b.say(t, message, "")
}

// How a message reached a task's agent.
const (
	saidLive  = "live"  // it reads it at its next step
	saidLater = "later" // its next turn, after the one running
	saidTurn  = "turn"  // a turn of its own, now
)

// say gives a task's agent a message: while it works, at its next step if
// it takes messages then, else in a turn after the one running; while it
// waits, in a turn that continues its conversation: a revision of the plan
// while it waits at Confirm, else of the work. from names who says it, an
// agent; "" for you.
func (b *board) say(t *Task, message, from string) string {
	if message = strings.TrimSpace(message); message == "" {
		return ""
	}
	shown := message
	if from != "" {
		shown = from + ": " + message
	}
	if _, running := b.runs[t.ID]; running || t.Status == Queued {
		if ch := b.inboxes[t.ID]; ch != nil {
			select {
			case ch <- message:
				t.addLog("› " + shown)
				b.save()
				return saidLive
			default:
			}
		}
		t.Asides = append(t.Asides, message)
		t.addLog("› " + shown + "  (after this turn)")
		b.save()
		return saidLater
	}
	if t.SessionRef == "" {
		return ""
	}
	plan := t.Status == Confirm || (t.Status == Failed && t.Stage == "plan")
	prompt := message
	if len(t.Asides) > 0 { // what it has not had yet comes first
		prompt = strings.Join(append(t.Asides, message), "\n\n")
		t.Asides = nil
	}
	if plan {
		prompt += reviseNote
	}
	b.turn(t, prompt, turnOpts{resume: true, plan: plan, said: shown})
	return saidTurn
}

// sendAsides gives the agent the messages that came while its turn ran, in
// a turn of their own, once the task has come to rest where a reply goes.
func (b *board) sendAsides(t *Task) bool {
	if len(t.Asides) == 0 || started(t) || t.SessionRef == "" || (t.Status != Review && t.Status != Confirm) {
		return false
	}
	msgs := t.Asides
	t.Asides = nil
	text := strings.Join(msgs, "\n\n")
	if len(msgs) > 1 {
		text = "Messages that came while you worked:\n\n" + text
	}
	plan := t.Status == Confirm
	prompt := text
	if plan {
		prompt += reviseNote
	}
	b.turn(t, prompt, turnOpts{resume: true, plan: plan, kind: "messages"})
	return started(t)
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
	b.autopick()
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
	if len(b.runs) >= b.maxRunning() && !t.Sub { // ponytail: a subtask runs past the limit, as the task waiting on it holds a slot
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
		t.Plans, t.Marks = nil, nil
	}
	var ws *Workspace
	if t.WS != nil {
		w := *t.WS
		ws = &w
	}
	t.Status, t.Err, t.Note = Running, "", ""
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
	if o.kind != "" {
		kind = o.kind
	}
	t.addLog(fmt.Sprintf("── %s · %s%s · %s", kind, agent, modelSuffix(model), time.Now().Format("15:04:05")))
	t.Doing = kind
	if o.reviewer == "" { // what the turn does, should the app quit under it
		t.Stage = "build"
		if o.plan {
			t.Stage = "plan"
		}
	}
	if o.said != "" {
		t.addLog("› " + o.said)
	}
	readOnly := o.plan || o.reviewer != "" || t.ReadOnly
	if readOnly && t.WS == nil && b.inPlace(t) {
		t.addLog("note: outside git, nothing undoes edits the agent makes anyway")
	}
	task, began := *t, time.Now() // the driver reads a copy, never the board
	if o.reviewer == "" {
		t.ResumeAt = "" // this turn goes on from there
	} else {
		task.ResumeAt = ""
	}
	task.Prompt, task.resume, task.readOnly = prompt, o.resume, readOnly
	if o.reviewer != "" { // the reviewer's own conversation, never the task's
		task.Agent, task.Model, task.SessionRef, task.resume = o.reviewer, "", "", false
		if o.reviewer == "claude" {
			task.SessionRef = newUUID()
			t.SessionIDs = append(t.SessionIDs, task.SessionRef) // the watcher leaves it to the task
		}
	}
	b.save()
	if liveAgents[agent] && o.reviewer == "" {
		ch := make(chan string, 32)
		b.inboxes[t.ID], task.inbox = ch, ch
	}
	closeBridge := func() {}
	if b.bridge != nil {
		task.mcpURL, closeBridge = b.bridge.open(t, o.reviewer != "")
	}
	b.procs.Add(1)
	go func() {
		defer b.procs.Done()
		defer closeBridge()
		var trim []string // the turn's folder as agents write it: tool lines show paths in it relative
		log := func(line string) {
			if strings.HasPrefix(line, "▸ ") {
				for _, d := range trim {
					line = strings.ReplaceAll(line, d, "")
				}
				line = clip(line, 160)
			}
			b.update(func() { t.addLog(line) })
		}
		if drop != nil {
			drop.remove()
		}
		var err error
		if ws != nil || (o.reviewer == "" && !t.InPlace) { // a review uses the task's worktree, or its folder
			ws, err = b.workspace(ctx, ws, task, log)
		}
		if err != nil {
			b.update(func() { b.finish(t, result{}, err, time.Since(began), before, o, turnGit{}, task) })
			return
		}
		head := ""
		if ws != nil {
			task.Dir = ws.Dir()
			head, _ = git(ws.Worktree, "rev-parse", "HEAD")
		}
		trim = spellings(task.Dir)
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
		g.head = head
		b.update(func() { b.finish(t, r, err, time.Since(began), before, o, g, task) })
	}()
}

// workspace readies the turn's worktree: the task's own, brought back if it
// was deleted, or a new one for its first turn, prepared. It runs off the
// main thread.
func (b *board) workspace(ctx context.Context, ws *Workspace, task Task, log emit) (*Workspace, error) {
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
	ws.prepare(ctx, log)
	w := *ws
	b.update(func() {
		for _, t := range b.Tasks {
			if t.ID == task.ID {
				t.WS = &w
			}
		}
		b.save() // should the app quit during the turn, its resume works here
	})
	return ws, nil
}

// turnGit is what a turn's checkpoint found.
type turnGit struct {
	ok        bool // a checkpoint ran; changes is the branch's
	changes   Changes
	discarded int    // files a planning turn changed, put back
	head      string // the commit the turn began on
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
			update(func() { b.quotas = q; b.autopick() })
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
	if t.WS == nil || t.merging || t.undoing || started(t) {
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

// publish pushes the task's branch and opens a pull request for it, or
// updates the one open, and shows it in your browser. Its description is
// what you asked for and the agent's last report.
func (b *board) publish(t *Task) {
	if t.WS == nil || t.pushing || t.merging || t.undoing || started(t) {
		return
	}
	t.pushing = true
	ws, title := *t.WS, t.Title
	body := "## Request\n\n" + t.Prompt
	if r := strings.TrimSpace(t.Result); r != "" {
		body += "\n\n## What " + t.Agent + " did\n\n" + r
	}
	go func() {
		url, err := ws.publish(title, body)
		if err == nil {
			exec.Command("open", url).Run()
		}
		b.update(func() {
			t.pushing = false
			if err != nil {
				t.Err = "pull request: " + err.Error()
				t.addLog("✗ " + t.Err)
			} else {
				if t.PR != url {
					t.addLog("── pull request " + url)
				}
				t.PR, t.Err = url, ""
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
func (b *board) finish(t *Task, r result, err error, took time.Duration, before Status, o turnOpts, g turnGit, task Task) {
	defer b.autopick() // then To Do, if Auto is on
	defer b.dispatch() // a slot is free: the next turn waiting takes it first
	select {           // what it spent shows in the quotas
	case b.quotaKick <- struct{}{}:
	default:
	}
	stopped := errors.Is(err, context.Canceled)
	if cancel := b.runs[t.ID]; cancel != nil {
		cancel()
		delete(b.runs, t.ID)
	}
	t.Asides = append(r.Unsent, t.Asides...) // what the agent never got waits for its next turn
	if ch := b.inboxes[t.ID]; ch != nil {
		delete(b.inboxes, t.ID)
	drain:
		for {
			select {
			case msg := <-ch:
				t.Asides = append(t.Asides, msg)
			default:
				break drain
			}
		}
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
	if r.SessionRef != "" && !slices.Contains(t.SessionIDs, r.SessionRef) {
		t.SessionIDs = append(t.SessionIDs, r.SessionRef) // the watcher leaves it to the task
	}
	if o.reviewer != "" {
		b.finishReview(t, r, err, before, o.reviewer)
		if !isStopped(err) && !b.sendAsides(t) {
			b.settled(t)
		}
		b.save()
		return
	}
	t.Usage.add(r.Usage)
	if r.Text != "" {
		t.Result = r.Text
	}
	if r.EndRef != "" && g.head != "" && rewindAgents[t.Agent] {
		t.Marks = append(t.Marks, TurnMark{Turn: task.Runs, Head: g.head, End: r.EndRef, First: !o.resume, Plan: o.plan})
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
	if !started(t) {
		t.picked = false // its agent is free again
	}
	if !stopped && !b.sendAsides(t) {
		b.settled(t)
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

// After the app quit under a turn: the agent's conversation is resumed once
// the board is up, in the same worktree, where what it did is still there.
const (
	resumedKind = "resumed"
	resumeNote  = "The board's app restarted while you were working, which stopped your last turn midway. " +
		"Continue the task from where you left off: first check what is already done, then finish the rest and report what you did."
	resumePlanNote = "\n\nYou were planning: still change no files, and reply with the whole plan."
)

// quitUnder sorts a task whose turn the app quit under, as the board loads.
// Its agent's conversation is resumed, unless it has none or the turn was
// itself a resume; an AI review goes back to its gate.
func (b *board) quitUnder(t *Task) {
	gate := Review
	if t.Stage == "plan" && len(t.Plans) > 0 {
		gate = Confirm
	}
	switch {
	case strings.HasPrefix(t.Doing, "review"):
		t.Status = gate
		t.addLog("── the AI review stopped: the app quit")
	case t.SessionRef != "" && t.Doing != resumedKind:
		t.Status = gate // where it rests should the resume be stopped
		b.interrupted = append(b.interrupted, t)
	default:
		t.Status, t.Err = Failed, "interrupted: the app quit while it ran"
	}
}

// resumeInterrupted resumes the turns the app quit under. It runs once the
// board can run turns.
func (b *board) resumeInterrupted() {
	for _, t := range b.interrupted {
		plan := t.Stage == "plan"
		prompt := resumeNote
		if plan {
			prompt += resumePlanNote
		}
		t.addLog("── the app quit during this task's turn: resuming it")
		b.turn(t, prompt, turnOpts{resume: true, plan: plan, kind: resumedKind})
	}
	b.interrupted = nil
}

// canUndo reports whether the task's last turn can be undone: its code put
// back, and the turn taken out of the agent's conversation.
func (b *board) canUndo(t *Task) bool {
	n := len(t.Marks)
	return n > 0 && t.WS != nil && !started(t) && !t.merging && !t.undoing && !b.shared(t) && (t.Marks[n-1].First || n > 1)
}

// undo takes the task back to before its last turn: the worktree to the
// commit the turn began on, and the conversation to where the turn before
// left it, which the next turn resumes at. Undoing the first turn leaves the
// task as if it never ran.
func (b *board) undo(t *Task) {
	if !b.canUndo(t) {
		return
	}
	m := t.Marks[len(t.Marks)-1]
	t.Marks = t.Marks[:len(t.Marks)-1]
	ws := *t.WS
	t.undoing = true
	go func() {
		_, err := ws.discard(m.Head)
		ch, _ := ws.changes()
		b.update(func() {
			t.undoing = false
			if err != nil {
				t.Err = "undo: " + err.Error()
				t.addLog("✗ " + t.Err)
				b.save()
				return
			}
			t.Changes, t.Merged, t.Err, t.Question = ch, false, "", nil
			if m.Plan && len(t.Plans) > 0 {
				t.Plans = t.Plans[:len(t.Plans)-1]
			}
			if m.First {
				t.SessionRef, t.ResumeAt, t.Result, t.Runs, t.Status, t.Marks = "", "", "", 0, Todo, nil
			} else {
				prev := t.Marks[len(t.Marks)-1]
				t.ResumeAt, t.Status = prev.End, Review
				if prev.Plan {
					t.Status = Confirm
				}
			}
			t.addLog(fmt.Sprintf("── undid turn %d: the code is back at %s, and the agent's conversation before it", m.Turn, m.Head[:min(12, len(m.Head))]))
			b.save()
		})
	}()
}

// takeOver makes a Claude Code session you ran in a terminal a task of the
// board: a reply resumes it, in its folder, where its work is. The watcher
// leaves it to the task from then on.
func (b *board) takeOver(s *Session) *Task {
	if s.Agent != "claude" || s.Working || s.ID == "" {
		return nil
	}
	b.NextID++
	t := &Task{ID: b.NextID, Title: s.Title, Prompt: s.Title, Agent: "claude", Model: s.Model, Dir: s.Dir, Status: Review, InPlace: true,
		SessionRef: s.ID, SessionIDs: []string{s.ID}, Runs: 1, Stage: "build", Doing: "terminal"}
	t.addLog("── taken over from a Claude Code session in a terminal (" + s.ID + "): replies resume it here")
	for _, line := range s.Recent {
		t.addLog(line)
	}
	b.Tasks = append(b.Tasks, t)
	b.sessions = slices.DeleteFunc(b.sessions, func(x *Session) bool { return x == s })
	b.open(t)
	b.save()
	return t
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

// setSessions takes a scan from the watcher, leaving out the board's own
// runs: the conversations its tasks hold, and any in its worktrees, where an
// agent that names its conversation only when the turn ends works meanwhile.
// ponytail: a task run in place outside git shows for its first turn only.
func (b *board) setSessions(all []*Session) {
	ours := map[string]bool{}
	for _, t := range b.Tasks {
		for _, id := range t.SessionIDs {
			ours[id] = true
		}
	}
	inWorktrees := func(dir string) bool {
		rel, err := filepath.Rel(b.worktrees, dir)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	b.sessions = slices.DeleteFunc(all, func(s *Session) bool { return ours[s.ID] || inWorktrees(s.Dir) })
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

// useShellPath gives an app opened from Finder or the Dock, which gets the
// system's bare PATH, the PATH of the user's login shell: the agents' CLIs
// are on it (~/.local/bin, Homebrew, npm). Run from a terminal, it has it.
func useShellPath() {
	if os.Getenv("TERM") != "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Interactive too: .zshrc is where many put PATH. Its prompts and
	// warnings print around the markers.
	out, err := exec.CommandContext(ctx, cmp.Or(os.Getenv("SHELL"), "/bin/zsh"), "-ilc", `printf '\n'"$1"'%s'"$1"'\n' "$PATH"`, "sh", pathMarker).Output()
	if p := markedPath(out); p != "" {
		os.Setenv("PATH", p+string(os.PathListSeparator)+os.Getenv("PATH"))
	} else {
		log.Printf("reading the login shell's PATH: %v", err)
	}
}

const pathMarker = "__AGENT_KANBAN_PATH__"

// markedPath finds the PATH the shell printed between the markers.
func markedPath(out []byte) string {
	_, rest, ok := strings.Cut(string(out), pathMarker)
	p, _, ok2 := strings.Cut(rest, pathMarker)
	if !ok || !ok2 {
		return ""
	}
	return strings.TrimSpace(p)
}

//go:embed assets/tray.png
var trayIcon []byte

func main() {
	useShellPath()
	dir, err := os.UserConfigDir()
	if err != nil {
		log.Fatal(err)
	}
	b := newBoard(filepath.Join(dir, "agent-kanban", "board.json"))
	// A fixed port, so agents outside the board keep its address; another
	// port if it is taken, and no server for them.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", outsidePort))
	fixed := err == nil
	if !fixed {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		log.Printf("the agents' MCP server: %v; they run without it", err)
	} else {
		br := newBridge(b)
		br.base = "http://" + ln.Addr().String() + "/mcp/"
		go http.Serve(ln, br)
		b.bridge = br
		if fixed {
			if b.OutsideToken == "" {
				b.OutsideToken = newUUID()
				b.save()
			}
			b.outsideURL = br.openOutside(b.OutsideToken)
		}
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
		b.focused, b.badge, b.copyText = win.IsFocused, mygo.App.SetBadgeCount, mygo.Clipboard.WriteText
		b.notify = func(n notice) {
			if !mygo.NotificationsSupported() { // `go run .`: only the packaged app can
				return
			}
			nt := mygo.NewNotification(mygo.NotificationOptions{ID: noticeID(n.ID), Title: n.Title, Subtitle: n.Subtitle, Body: n.Body})
			go func() {
				if err := nt.Show(); err != nil {
					log.Printf("notification: %v", err)
				}
			}()
		}
		mygo.App.OnNotificationClick(func(id string) {
			win.Update(func() { b.openNotice(id) })
			win.Show()
			win.Focus()
		})
		b.resumeInterrupted()
		show := func() { win.Show(); win.Focus() }
		if tray, err := mygo.NewTray(mygo.TrayOptions{ID: "agent-kanban", Icon: trayIcon, IconIsTemplate: true, ToolTip: "Agent Kanban"}); err != nil {
			log.Printf("the menu bar icon: %v", err)
		} else {
			b.tray = func() {
				title := ""
				if n := len(b.unread()); n > 0 {
					title = fmt.Sprint(n)
				}
				tray.SetTitle(title)
				tray.SetMenu(mygo.NewMenu(b.trayMenu(show)))
			}
			b.refreshTray()
		}
		go func() { // the schedules
			for {
				win.Update(func() { b.fireDue(time.Now()) })
				time.Sleep(30 * time.Second)
			}
		}()
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
