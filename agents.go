package main

// The three agent drivers. Each launches the vendor CLI the user already
// installed and signed in to, in its headless streaming mode, and turns its
// output into log lines and token usage.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
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
}

// emit receives a log line as the agent works. It is called from the run's
// goroutine.
type emit func(line string)

type driver func(ctx context.Context, t Task, log emit) (result, error)

var drivers = map[string]driver{
	"claude":      runClaude,
	"antigravity": runAntigravity,
	"copilot":     runCopilot,
}

var agentNames = []string{"claude", "antigravity", "copilot"}

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

// toolLine formats a tool call: its name and its most telling argument.
func toolLine(name string, args map[string]any) string {
	for _, k := range []string{"command", "file_path", "path", "pattern", "url", "query", "description"} {
		if v, ok := args[k].(string); ok && v != "" {
			return "▸ " + name + " " + firstLine(v)
		}
	}
	return "▸ " + name
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
// the board's, or resumes it: --resume keeps its id.
func claudeArgs(t Task) []string {
	args := []string{"-p", t.Prompt, "--output-format", "stream-json", "--verbose", "--permission-mode", "bypassPermissions"}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if t.readOnly { // Bash can still write: what it leaves is undone after the turn
		args = append(args, "--disallowedTools=Write,Edit,NotebookEdit")
	}
	if t.SessionRef != "" {
		if t.resume {
			args = append(args, "--resume", t.SessionRef)
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
	out, err := cmd.StdoutPipe()
	if err != nil {
		return result{}, err
	}
	if err := cmd.Start(); err != nil {
		return result{}, err
	}
	var r result
	for sc := newScanner(out); sc.Scan(); {
		claudeLine(sc.Bytes(), &r, log)
	}
	if err := cmd.Wait(); err != nil && r.Err == "" {
		return r, exitErr(err, stderr)
	}
	return r, nil
}

func claudeLine(line []byte, r *result, log emit) {
	var m struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string         `json:"type"`
				Text  string         `json:"text"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			} `json:"content"`
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
	switch m.Type {
	case "assistant":
		if m.ParentToolUseID != nil { // a subagent's message, not the main loop's
			return
		}
		for _, b := range m.Message.Content {
			switch b.Type {
			case "text":
				log(b.Text)
			case "tool_use":
				log(toolLine(b.Name, b.Input))
			}
		}
	case "result":
		r.Text = m.Result
		for _, u := range m.ModelUsage {
			r.Usage.add(Usage{Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens})
		}
		r.Usage.CostUSD = m.CostUSD
		if m.IsError {
			r.Err = "Claude Code: " + firstLine(m.Result+" "+m.Subtype)
		}
	}
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

// rpc is a JSON-RPC 2.0 client over Content-Length framed stdio.
type rpc struct {
	w       io.Writer
	wmu     sync.Mutex
	mu      sync.Mutex
	next    int
	pending map[int]chan rpcReply
	closed  chan struct{}
}

type rpcReply struct {
	Result json.RawMessage
	Error  *struct {
		Message string `json:"message"`
	}
}

func newRPC(w io.Writer, r io.Reader, notify func(method string, params json.RawMessage)) *rpc {
	c := &rpc{w: w, pending: map[int]chan rpcReply{}, closed: make(chan struct{})}
	go func() {
		defer close(c.closed)
		br := bufio.NewReader(r)
		for {
			body, err := readFrame(br)
			if err != nil {
				return
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
			case m.ID != nil && m.Method != "": // a request from the server: nobody is here to answer it
				c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "unattended: " + m.Method}})
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
	return c
}

func (c *rpc) send(msg any) error {
	body, _ := json.Marshal(msg)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}

func (c *rpc) call(ctx context.Context, method string, params, out any) error {
	ch := make(chan rpcReply, 1)
	c.mu.Lock()
	c.next++
	id := c.next
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case rep := <-ch:
		if rep.Error != nil {
			return fmt.Errorf("%s: %s", method, rep.Error.Message)
		}
		if out != nil {
			return json.Unmarshal(rep.Result, out)
		}
		return nil
	case <-c.closed:
		return fmt.Errorf("%s: copilot exited", method)
	case <-ctx.Done():
		return ctx.Err()
	}
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
