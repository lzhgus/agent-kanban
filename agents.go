package main

// The agent drivers. Each launches the vendor CLI the user already
// installed and signed in to, in its headless streaming mode, and turns its
// output into log lines and token usage.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Usage is what one run spent. Only Claude Code reports a price; the others
// count tokens against a subscription.
type Usage struct {
	Input, Output, CacheRead, CacheWrite int
	CostUSD                              float64
}

func (u Usage) total() int { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

func (u *Usage) add(v Usage) {
	u.Input += v.Input
	u.Output += v.Output
	u.CacheRead += v.CacheRead
	u.CacheWrite += v.CacheWrite
	u.CostUSD += v.CostUSD
}

// result accumulates one turn's outcome while its output streams in.
type result struct {
	Usage      Usage
	Text       string // the final answer, handed to the next agent on a handoff
	Err        string
	SessionRef string // the agent's conversation, to resume on the next turn
	// Unsent are messages the turn took in while it ran but could not pass
	// to the agent: they go in its next turn.
	Unsent []string
	// EndRef is where the turn left the agent's conversation, for a later
	// turn to take it back to (rewindAgents).
	EndRef string
}

// emit receives a log line as the agent works. It is called from the run's
// goroutine.
type emit func(line string)

type driver func(ctx context.Context, t Task, log emit) (result, error)

var drivers = map[string]driver{
	"claude":      runClaude,
	"antigravity": runAntigravity,
	"copilot":     runCopilot,
	"grok":        runGrok,
	"pi":          runPi,
	"opencode":    runOpencode,
	"kimi":        runKimi,
}

var agentNames = []string{"claude", "antigravity", "copilot", "grok", "pi", "opencode", "kimi"}

// rewindAgents can resume their conversation at an earlier point (EndRef),
// so a turn can be undone.
var rewindAgents = map[string]bool{"claude": true}

// liveAgents take messages while a turn runs, from Task.inbox: the agent
// reads them at its next step. Others get them in a turn after the one
// running.
var liveAgents = map[string]bool{"claude": true, "grok": true, "pi": true}

// command starts bin in its own process group, so stopping a run also ends
// the tools the agent spawned.
// ponytail: unix only (Setpgid); a Windows build needs a job object here.
func command(ctx context.Context, cwd, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 15 * time.Second
	return cmd
}

// agentHome is a folder of the board's own for an agent's config, so the
// agent runs apart from the user's: their rules, plugins and servers stay
// out, and only the sign-in is shared.
func agentHome(name string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	home := filepath.Join(dir, "agent-kanban", name)
	return home, os.MkdirAll(home, 0o700)
}

// share links src, a sign-in file of the user's, into dir, so a token the
// agent refreshes is the user's too. It reports whether src exists.
func share(src, dir string) bool {
	if _, err := os.Stat(src); err != nil {
		return false
	}
	dst := filepath.Join(dir, filepath.Base(src))
	if fi, err := os.Lstat(dst); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return true
	}
	os.Remove(dst)
	if os.Symlink(src, dst) != nil { // a copy then: a refreshed token stays behind
		if data, err := os.ReadFile(src); err == nil {
			os.WriteFile(dst, data, 0o600)
		}
	}
	return true
}

// stderrTail keeps the end of a process's stderr for the error message.
type stderrTail struct {
	mu  sync.Mutex
	buf []byte
}

func (s *stderrTail) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	if len(s.buf) > 4096 {
		s.buf = s.buf[len(s.buf)-4096:]
	}
	return len(p), nil
}

func (s *stderrTail) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(string(s.buf))
}

func exitErr(err error, stderr *stderrTail) error {
	if msg := stderr.String(); msg != "" {
		lines := strings.Split(msg, "\n")
		return fmt.Errorf("%w: %s", err, lines[len(lines)-1])
	}
	return err
}

func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // tool results can be large lines
	return sc
}

// toolLine formats a tool call: its name, as Claude Code would name it, and
// its most telling argument.
func toolLine(name string, args map[string]any) string {
	name = toolName(name)
	for _, k := range []string{"command", "file_path", "filePath", "path", "pattern", "url", "query", "description"} {
		if v, ok := args[k].(string); ok && v != "" {
			return "▸ " + name + " " + firstLine(v)
		}
	}
	return "▸ " + name
}

// toolNames maps each agent's names for the same tools to one set.
var toolNames = map[string]string{
	"bash": "Bash", "shell": "Bash", "run_command": "Bash", "run_terminal_command": "Bash", "run_terminal_cmd": "Bash",
	"read": "Read", "read_file": "Read", "view": "Read",
	"write": "Write", "write_file": "Write", "create_file": "Write", "create": "Write",
	"edit": "Edit", "search_replace": "Edit", "patch": "Edit", "apply_patch": "Edit", "str_replace": "Edit",
	"grep": "Grep", "glob": "Glob", "list_dir": "Glob",
	"web_fetch": "WebFetch", "webfetch": "WebFetch", "web_search": "WebSearch", "websearch": "WebSearch",
	"task": "Task", "subagent": "Task", "Agent": "Task",
	"search_tool": "ToolSearch", "tool_search": "ToolSearch",
}

// toolName names a tool for the log: one name for the same tool across
// agents, and an MCP server's tool as server/tool, however the agent
// prefixes it (mcp__kanban__ask_user, kanban__ask_user, kanban_ask_user,
// kanban-ask_user).
func toolName(name string) string {
	if n, ok := toolNames[name]; ok {
		return n
	}
	for _, server := range []string{bridgeName, outsideName} {
		for _, sep := range []string{"__", "_", "-"} {
			if tool, ok := strings.CutPrefix(strings.TrimPrefix(name, "mcp__"), server+sep); ok && tool != "" {
				return server + "/" + tool
			}
		}
	}
	if server, tool, ok := strings.Cut(strings.TrimPrefix(name, "mcp__"), "__"); ok && server != "" && tool != "" { // any other server's
		return server + "/" + tool
	}
	return name
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// ── Claude Code ─────────────────────────────────────────────────────────────
// `claude -p --output-format stream-json --verbose`: one JSON message per
// line, ending with a "result" that carries per-model usage and the cost.

// claudeArgs starts the session the board named, so the watcher knows it is
// the board's, or resumes it: --resume keeps its id. A turn that takes
// messages while it runs reads its prompt, and them, from stdin; Claude Code
// echoes each as it takes it in.
func claudeArgs(t Task) []string {
	args := []string{"-p", t.Prompt}
	if t.inbox != nil {
		args = []string{"-p", "--input-format", "stream-json", "--replay-user-messages"}
	}
	args = append(args, "--output-format", "stream-json", "--verbose", "--permission-mode", "bypassPermissions")
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if t.mcpURL != "" { // next to the user's own servers, which still load
		cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{bridgeName: map[string]any{"type": "http", "url": t.mcpURL}}})
		args = append(args, "--mcp-config="+string(cfg))
	}
	denied := "mcp__" + outsideName // the board's server for agents outside it: a turn has its own
	if t.readOnly {                 // Bash can still write: what it leaves is undone after the turn
		denied = "Write,Edit,NotebookEdit," + denied
	}
	args = append(args, "--disallowedTools="+denied)
	if t.SessionRef != "" {
		if t.resume {
			args = append(args, "--resume", t.SessionRef)
			if t.ResumeAt != "" { // the turns after it were undone
				args = append(args, "--resume-session-at", t.ResumeAt)
			}
		} else {
			args = append(args, "--session-id", t.SessionRef)
		}
	}
	return args
}

func runClaude(ctx context.Context, t Task, log emit) (result, error) {
	cmd := command(ctx, t.Dir, "claude", claudeArgs(t)...)
	stderr := &stderrTail{}
	cmd.Stderr = stderr
	var in io.WriteCloser
	if t.inbox != nil {
		var err error
		if in, err = cmd.StdinPipe(); err != nil {
			return result{}, err
		}
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return result{}, err
	}
	if err := cmd.Start(); err != nil {
		return result{}, err
	}
	var r result
	if in == nil {
		for sc := newScanner(out); sc.Scan(); {
			claudeLine(sc.Bytes(), &r, log)
		}
	} else {
		claudeLive(t, in, out, &r, log)
	}
	if err := cmd.Wait(); err != nil && r.Err == "" {
		return r, exitErr(err, stderr)
	}
	return r, nil
}

// claudeLive runs a turn that takes messages while it works: the prompt and
// each message go in as user messages, and Claude Code folds a message into
// the work at its next step. Once a result comes with every message taken
// in, stdin closes and the turn ends; a message that came too late for the
// work runs on after it, in the same process, ending in a result of its own.
func claudeLive(t Task, in io.WriteCloser, out io.Reader, r *result, log emit) {
	lines := make(chan string)
	go func() {
		defer close(lines)
		for sc := newScanner(out); sc.Scan(); {
			lines <- sc.Text()
		}
	}()
	inbox, pending := t.inbox, map[string]bool{}
	send := func(text string) bool {
		id := newUUID() // unique in the conversation: Claude Code ignores a message whose id it has seen
		msg, _ := json.Marshal(map[string]any{"type": "user", "uuid": id, "message": map[string]any{"role": "user", "content": text}})
		if _, err := in.Write(append(msg, '\n')); err != nil {
			return false
		}
		pending[id] = true
		return true
	}
	if !send(t.Prompt) {
		in.Close()
		for range lines {
		}
		return
	}
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				in.Close()
				return
			}
			ev := claudeLine([]byte(line), r, log)
			delete(pending, ev.took)
			if ev.result && len(pending) == 0 && inbox != nil {
				in.Close() // done: what comes now waits for a turn of its own
				inbox = nil
			}
		case msg := <-inbox:
			if !send(msg) {
				r.Unsent = append(r.Unsent, msg)
			}
		}
	}
}

// claudeEvent is what a line of Claude Code's output meant for the turn.
type claudeEvent struct {
	took   string // the id of the message Claude Code took in
	result bool   // the work so far ended
}

func claudeLine(line []byte, r *result, log emit) (ev claudeEvent) {
	var m struct {
		Type     string `json:"type"`
		UUID     string `json:"uuid"`
		IsReplay bool   `json:"isReplay"`
		Message  struct {
			Content json.RawMessage `json:"content"` // blocks; a string in a user message echoed back
		} `json:"message"`
		SessionID       string  `json:"session_id"`
		ParentToolUseID *string `json:"parent_tool_use_id"`
		IsError         bool    `json:"is_error"`
		Result          string  `json:"result"`
		Subtype         string  `json:"subtype"`
		CostUSD         float64 `json:"total_cost_usd"`
		ModelUsage      map[string]struct {
			InputTokens              int `json:"inputTokens"`
			OutputTokens             int `json:"outputTokens"`
			CacheReadInputTokens     int `json:"cacheReadInputTokens"`
			CacheCreationInputTokens int `json:"cacheCreationInputTokens"`
		} `json:"modelUsage"`
	}
	if json.Unmarshal(line, &m) != nil {
		return
	}
	if m.SessionID != "" { // the session exists: a reply can resume it
		r.SessionRef = m.SessionID
	}
	if (m.Type == "assistant" || m.Type == "user") && m.UUID != "" && m.ParentToolUseID == nil {
		r.EndRef = m.UUID // the conversation so far ends here
	}
	switch m.Type {
	case "user":
		if m.IsReplay {
			ev.took = m.UUID
		}
	case "assistant":
		if m.ParentToolUseID != nil { // a subagent's message, not the main loop's
			return
		}
		var blocks []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		}
		json.Unmarshal(m.Message.Content, &blocks)
		for _, b := range blocks {
			switch b.Type {
			case "text":
				log(b.Text)
			case "tool_use":
				log(toolLine(b.Name, b.Input))
			}
		}
	case "result": // its usage and cost count the whole process, every result so far
		ev.result = true
		r.Text, r.Usage, r.Err = m.Result, Usage{}, ""
		for _, u := range m.ModelUsage {
			r.Usage.add(Usage{Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens})
		}
		r.Usage.CostUSD = m.CostUSD
		if m.IsError {
			r.Err = "Claude Code: " + firstLine(m.Result+" "+m.Subtype)
		}
	}
	return ev
}

// ── Antigravity ─────────────────────────────────────────────────────────────
// `agy --input-format stream-json --output-format stream-json`: the prompt
// goes in as {"event":"user"}; out come "init", "step_update"s and a
// "result". agy exits by itself a few seconds after stdin closes.

func agyArgs(t Task) []string {
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--dangerously-skip-permissions"}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if t.resume && t.SessionRef != "" {
		args = append(args, "--conversation", t.SessionRef)
	}
	return args
}

func runAntigravity(ctx context.Context, t Task, log emit) (result, error) {
	cmd := command(ctx, t.Dir, "agy", agyArgs(t)...)
	stderr := &stderrTail{}
	cmd.Stderr = stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return result{}, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return result{}, err
	}
	if err := cmd.Start(); err != nil {
		return result{}, err
	}
	msg, _ := json.Marshal(map[string]any{"event": "user", "message": map[string]string{"content": t.Prompt}})
	if _, err := in.Write(append(msg, '\n')); err != nil {
		_ = cmd.Wait()
		return result{}, exitErr(err, stderr)
	}
	var r result
	s := &agyStream{counted: map[int]bool{}, announced: map[int]bool{}}
	for sc := newScanner(out); sc.Scan(); {
		if s.line(sc.Bytes(), &r, log) {
			in.Close() // the turn is over; let agy save and exit
		}
	}
	in.Close()
	if err := cmd.Wait(); err != nil && !s.done {
		return r, exitErr(err, stderr)
	}
	return r, nil
}

type agyStream struct {
	text      strings.Builder // the answer streaming in
	counted   map[int]bool    // steps whose usage was added
	announced map[int]bool    // steps whose tool call was logged
	done      bool
}

// line handles one event, reporting whether it ended the turn.
func (s *agyStream) line(line []byte, r *result, log emit) bool {
	var m struct {
		Event          string `json:"event"`
		ConversationID string `json:"conversation_id"`
		Step           struct {
			StepType  string `json:"step_type"`
			StepIndex int    `json:"step_index"`
			State     string `json:"state"`
			TextDelta string `json:"text_delta"`
			ToolName  string `json:"tool_name"`
			ToolInfo  struct {
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"tool_info"`
			Usage *struct {
				Input     int `json:"input_tokens"`
				Output    int `json:"output_tokens"`
				CacheRead int `json:"cache_read_tokens"`
			} `json:"usage"`
		} `json:"step_update"`
		Result struct {
			Error string `json:"error"`
		} `json:"result"`
	}
	if json.Unmarshal(line, &m) != nil {
		return false
	}
	st := m.Step
	switch m.Event {
	case "init": // the conversation agy opened or resumed
		if m.ConversationID != "" {
			r.SessionRef = m.ConversationID
		}
	case "step_update":
		switch st.StepType {
		case "agent_response":
			s.text.WriteString(st.TextDelta)
			if st.State != "ACTIVE" {
				s.flush(r, log)
			}
		case "tool", "subagent":
			s.flush(r, log)
			if !s.announced[st.StepIndex] {
				s.announced[st.StepIndex] = true
				name := st.ToolName
				if name == "" {
					name = st.ToolInfo.Name
				}
				log(toolLine(name, st.ToolInfo.Parameters))
			}
		}
		if st.Usage != nil && st.State != "ACTIVE" && !s.counted[st.StepIndex] {
			s.counted[st.StepIndex] = true
			r.Usage.add(Usage{Input: st.Usage.Input, Output: st.Usage.Output, CacheRead: st.Usage.CacheRead})
		}
	case "result":
		s.flush(r, log)
		if m.Result.Error != "" {
			r.Err = "Antigravity: " + firstLine(m.Result.Error)
		}
		s.done = true
		return true
	}
	return false
}

func (s *agyStream) flush(r *result, log emit) {
	if t := strings.TrimSpace(s.text.String()); t != "" {
		log(t)
		r.Text = t
	}
	s.text.Reset()
}

// ── GitHub Copilot ──────────────────────────────────────────────────────────
// `copilot --headless --stdio`: the JSON-RPC server Copilot's own SDK drives,
// framed as LSP is (Content-Length headers). A turn is session.create,
// session.send, then "session.event" notifications until "session.idle".

func runCopilot(ctx context.Context, t Task, log emit) (result, error) {
	cmd := command(ctx, t.Dir, "copilot", "--headless", "--no-auto-update", "--stdio")
	stderr := &stderrTail{}
	cmd.Stderr = stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return result{}, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return result{}, err
	}
	if err := cmd.Start(); err != nil {
		return result{}, err
	}
	var (
		r    result
		s    copilotStream
		mu   sync.Mutex // guards r and s, written by the reader goroutine
		idle = make(chan struct{}, 1)
	)
	c := newRPC(in, out, func(method string, params json.RawMessage) {
		if method != "session.event" {
			return
		}
		mu.Lock()
		ended := s.event(params, &r, log)
		mu.Unlock()
		if ended {
			select {
			case idle <- struct{}{}:
			default:
			}
		}
	})
	defer func() {
		in.Close()
		_ = cmd.Wait()
	}()
	fail := func(err error) (result, error) { return result{}, exitErr(err, stderr) }

	var hello struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if err := c.call(ctx, "connect", map[string]any{}, &hello); err != nil {
		return fail(err)
	}
	if hello.ProtocolVersion < 3 {
		return result{}, errors.New("GitHub Copilot CLI is too old to drive headless — run `copilot update`")
	}
	opts := map[string]any{"workingDirectory": t.Dir, "streaming": true, "requestPermission": true}
	if t.Model != "" {
		opts["model"] = t.Model
	}
	if t.mcpURL != "" {
		opts["mcpServers"] = map[string]any{bridgeName: map[string]any{"type": "http", "url": t.mcpURL, "headers": map[string]string{}}}
	}
	id := ""
	if t.resume && t.SessionRef != "" {
		resume := map[string]any{"sessionId": t.SessionRef}
		for k, v := range opts {
			resume[k] = v
		}
		if err := c.call(ctx, "session.resume", resume, nil); err != nil {
			if ctx.Err() != nil {
				return result{}, ctx.Err()
			}
			log("couldn't resume the Copilot session (" + firstLine(err.Error()) + "); starting a new one")
		} else {
			id = t.SessionRef
		}
	}
	if id == "" {
		var created struct {
			SessionID string `json:"sessionId"`
		}
		if err := c.call(ctx, "session.create", opts, &created); err != nil {
			return fail(err)
		}
		id = created.SessionID
	}
	if err := c.call(ctx, "session.permissions.setApproveAll", map[string]any{"sessionId": id, "enabled": true}, nil); err != nil {
		return fail(err)
	}
	mu.Lock()
	s.started = true // only events of this turn count
	mu.Unlock()
	if err := c.call(ctx, "session.send", map[string]any{"sessionId": id, "prompt": t.Prompt}, nil); err != nil {
		return fail(err)
	}
	select {
	case <-idle:
	case <-c.closed:
		return fail(errors.New("copilot exited before the turn ended"))
	case <-ctx.Done():
		return result{}, ctx.Err()
	}
	mu.Lock()
	defer mu.Unlock()
	r.SessionRef = id
	return r, nil
}

type copilotStream struct {
	started bool
	message string // id of the assistant message streaming in
	text    strings.Builder
}

// event handles one session event, reporting whether the turn is over.
func (s *copilotStream) event(params json.RawMessage, r *result, log emit) bool {
	var n struct {
		Event struct {
			Type    string          `json:"type"`
			AgentID string          `json:"agentId"`
			Data    json.RawMessage `json:"data"`
		} `json:"event"`
	}
	if json.Unmarshal(params, &n) != nil || !s.started || n.Event.AgentID != "" {
		return false // subagents' events, and those before the turn, are not ours
	}
	var d struct {
		MessageID    string         `json:"messageId"`
		DeltaContent string         `json:"deltaContent"`
		Content      string         `json:"content"`
		ToolName     string         `json:"toolName"`
		Arguments    map[string]any `json:"arguments"`
		Message      string         `json:"message"`
		Input        int            `json:"inputTokens"`
		Output       int            `json:"outputTokens"`
		CacheRead    int            `json:"cacheReadTokens"`
		CacheWrite   int            `json:"cacheWriteTokens"`
	}
	_ = json.Unmarshal(n.Event.Data, &d)
	switch n.Event.Type {
	case "assistant.message_delta":
		if d.MessageID != s.message {
			s.flush(r, log)
			s.message = d.MessageID
		}
		s.text.WriteString(d.DeltaContent)
	case "assistant.message": // the finished message replaces its deltas
		s.text.Reset()
		s.text.WriteString(d.Content)
		s.flush(r, log)
		s.message = ""
	case "tool.execution_start":
		s.flush(r, log)
		log(toolLine(d.ToolName, d.Arguments))
	case "session.error":
		r.Err = "GitHub Copilot: " + firstLine(d.Message)
	case "assistant.usage":
		// inputTokens counts cache reads and writes too; keep them apart.
		r.Usage.add(Usage{Input: max(0, d.Input-d.CacheRead-d.CacheWrite), Output: d.Output, CacheRead: d.CacheRead, CacheWrite: d.CacheWrite})
	case "session.idle":
		s.flush(r, log)
		return true
	}
	return false
}

func (s *copilotStream) flush(r *result, log emit) {
	if t := strings.TrimSpace(s.text.String()); t != "" {
		log(t)
		r.Text = t
	}
	s.text.Reset()
}

// rpc is a JSON-RPC 2.0 client over stdio: Content-Length framed, as
// Copilot's server is, or one message per line, as the Agent Client
// Protocol is.
type rpc struct {
	w       io.Writer
	wmu     sync.Mutex
	mu      sync.Mutex
	next    int
	pending map[int]chan rpcReply
	closed  chan struct{}
	label   string // the agent, for errors
	lines   bool
	// serve answers a request from the agent; nil, or a nil answer, declines it.
	serve func(method string, params json.RawMessage) any
}

type rpcReply struct {
	Result json.RawMessage
	Error  *struct {
		Message string `json:"message"`
	}
}

func newRPC(w io.Writer, r io.Reader, notify func(method string, params json.RawMessage)) *rpc {
	c := &rpc{w: w, label: "copilot"}
	c.start(r, notify)
	return c
}

// newACP speaks the Agent Client Protocol: JSON-RPC, a message a line.
func newACP(w io.Writer, r io.Reader, label string, notify func(method string, params json.RawMessage), serve func(method string, params json.RawMessage) any) *rpc {
	c := &rpc{w: w, label: label, lines: true, serve: serve}
	c.start(r, notify)
	return c
}

func (c *rpc) start(r io.Reader, notify func(method string, params json.RawMessage)) {
	c.pending, c.closed = map[int]chan rpcReply{}, make(chan struct{})
	go func() {
		defer close(c.closed)
		br, sc := bufio.NewReader(r), newScanner(r) // one or the other reads
		for {
			var body []byte
			if c.lines {
				if !sc.Scan() {
					return
				}
				body = sc.Bytes()
			} else {
				var err error
				if body, err = readFrame(br); err != nil {
					return
				}
			}
			var m struct {
				ID     *json.RawMessage `json:"id"`
				Method string           `json:"method"`
				Params json.RawMessage  `json:"params"`
				rpcReply
			}
			if json.Unmarshal(body, &m) != nil {
				continue
			}
			switch {
			case m.ID != nil && m.Method != "": // a request from the agent
				var answer any
				if c.serve != nil {
					answer = c.serve(m.Method, m.Params)
				}
				if answer != nil {
					c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": answer})
				} else { // nobody is here to answer it
					c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "unattended: " + m.Method}})
				}
			case m.ID != nil:
				id, _ := strconv.Atoi(string(*m.ID))
				c.mu.Lock()
				ch := c.pending[id]
				delete(c.pending, id)
				c.mu.Unlock()
				if ch != nil {
					ch <- m.rpcReply
				}
			default:
				notify(m.Method, m.Params)
			}
		}
	}()
}

func (c *rpc) send(msg any) error {
	body, _ := json.Marshal(msg)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.lines {
		_, err := c.w.Write(append(body, '\n'))
		return err
	}
	_, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}

func (c *rpc) call(ctx context.Context, method string, params, out any) error {
	ch, err := c.request(method, params)
	if err != nil {
		return err
	}
	return c.wait(ctx, method, ch, out)
}

// request sends a call, and returns where its reply comes.
func (c *rpc) request(method string, params any) (chan rpcReply, error) {
	ch := make(chan rpcReply, 1)
	c.mu.Lock()
	c.next++
	id := c.next
	c.pending[id] = ch
	c.mu.Unlock()
	return ch, c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

// wait waits for the reply to a request, into out.
func (c *rpc) wait(ctx context.Context, method string, ch chan rpcReply, out any) error {
	var rep rpcReply
	select {
	case rep = <-ch:
	case <-c.closed:
		select {
		case rep = <-ch: // it replied, then exited
		default:
			return fmt.Errorf("%s: %s exited", method, c.label)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	if rep.Error != nil {
		return fmt.Errorf("%s: %s", method, rep.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(rep.Result, out)
	}
	return nil
}

func readFrame(br *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			if length >= 0 {
				break
			}
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "Content-Length") {
			length, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	body := make([]byte, length)
	_, err := io.ReadFull(br, body)
	return body, err
}
