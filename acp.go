package main

// ── Agent Client Protocol ───────────────────────────────────────────────────
// Grok Build, Kimi Code and OpenCode speak ACP over stdio: JSON-RPC, a
// message a line. A turn is initialize, session/new (or session/load to
// resume), then session/prompt, whose reply ends it; the work streams in as
// session/update. Each agent's spec says how it differs: its command and
// home, how it runs unattended, whether it takes a message mid-turn, and
// where it keeps its token counts.

import (
	"cmp"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
)

type acpAgent struct {
	label string // its name in errors
	bin   string
	// args are its command line, with the task's model when it takes it
	// there; otherwise the model is set on the session.
	args       func(model string) []string
	modelInArg bool
	// env readies the agent's own home for a task and returns the process
	// environment to run in it.
	env func(t Task) ([]string, error)
	// meta goes with session/new and session/load; mode is the session
	// mode it runs in, unattended.
	meta map[string]any
	mode string
	// allow answers a permission request yes, for an agent that asks
	// whatever its mode; others are told no (they run with every tool
	// approved, so what still asks is a rule of the repository's).
	allow bool
	// interject is the method that hands it a message mid-turn; without
	// one, messages wait for the next turn.
	interject string
	// usage is where it keeps a turn's token counts, when neither its
	// updates nor the prompt's reply carry them: called before the turn, it
	// returns what to call after.
	usage func(env []string, sessionID string) func() Usage
}

var acpInitialize = map[string]any{
	"protocolVersion":    1,
	"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false},
	"clientInfo":         map[string]string{"name": "agent-kanban", "version": "1"},
}

// serve answers what the agent asks while nobody is at the keyboard.
func (a *acpAgent) serve(method string, params json.RawMessage) any {
	switch method {
	case "session/request_permission":
		var p struct {
			Options []struct {
				OptionID string `json:"optionId"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		json.Unmarshal(params, &p)
		kinds := []string{"reject_once", "reject_always"}
		if a.allow {
			kinds = []string{"allow_always", "allow_once"}
		}
		for _, kind := range kinds {
			for _, o := range p.Options {
				if o.Kind == kind {
					return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": o.OptionID}}
				}
			}
		}
		return map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}
	case "_x.ai/mcp/elicit": // an MCP server's question
		return map[string]string{"action": "decline"}
	}
	return nil
}

// start launches the agent in dir and connects to it; stop ends it.
func (a *acpAgent) start(ctx context.Context, dir, model string, env []string, notify func(string, json.RawMessage)) (c *rpc, stop func(), stderr *stderrTail, err error) {
	if !a.modelInArg {
		model = ""
	}
	cmd := command(ctx, dir, a.bin, a.args(model)...)
	cmd.Env = env
	stderr = &stderrTail{}
	cmd.Stderr = stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	c = newACP(in, out, a.bin, notify, a.serve)
	return c, func() { in.Close(); _ = cmd.Wait() }, stderr, nil
}

// stopped says why a turn that ended this way did no work.
var stopped = map[string]string{
	"refusal":    "refused the request",
	"max_tokens": "stopped: the answer reached its length limit",
	"cancelled":  "stopped the turn",
}

func (a *acpAgent) run(ctx context.Context, t Task, log emit) (result, error) {
	env, err := a.env(t)
	if err != nil {
		return result{}, err
	}
	var (
		r  result
		s  = acpStream{dirs: spellings(t.Dir), tools: map[string]*acpTool{}}
		mu sync.Mutex // guards r and s, written by the reader goroutine
	)
	c, stop, stderr, err := a.start(ctx, t.Dir, t.Model, env, func(method string, params json.RawMessage) {
		mu.Lock()
		s.update(method, params, &r, log)
		mu.Unlock()
	})
	if err != nil {
		return result{}, err
	}
	defer stop()
	fail := func(err error) (result, error) {
		if ctx.Err() != nil {
			return result{}, ctx.Err()
		}
		return result{}, exitErr(err, stderr)
	}
	if err := c.call(ctx, "initialize", acpInitialize, nil); err != nil {
		return fail(err)
	}
	mcp := []any{}
	if t.mcpURL != "" {
		mcp = append(mcp, map[string]any{"type": "http", "name": bridgeName, "url": t.mcpURL, "headers": []any{}})
	}
	open := map[string]any{"cwd": t.Dir, "mcpServers": mcp}
	if a.meta != nil {
		open["_meta"] = a.meta
	}
	id := ""
	if t.resume && t.SessionRef != "" { // it replays the conversation first: not this turn's
		open["sessionId"] = t.SessionRef
		err := c.call(ctx, "session/load", open, nil)
		delete(open, "sessionId")
		if err == nil {
			id = t.SessionRef
		} else if ctx.Err() != nil {
			return result{}, ctx.Err()
		} else {
			log("couldn't resume the " + a.label + " session (" + firstLine(err.Error()) + "); starting a new one")
		}
	}
	if id == "" {
		var created struct {
			SessionID string `json:"sessionId"`
		}
		if err := c.call(ctx, "session/new", open, &created); err != nil {
			return fail(err)
		}
		id = created.SessionID
	}
	set := func(option, value string) error {
		return c.call(ctx, "session/set_config_option", map[string]any{"sessionId": id, "configId": option, "value": value}, nil)
	}
	if a.mode != "" {
		if err := set("mode", a.mode); err != nil {
			log("couldn't set " + a.label + " to run unattended: " + firstLine(err.Error()))
		}
	}
	if t.Model != "" && !a.modelInArg {
		if err := set("model", t.Model); err != nil {
			return fail(err)
		}
	}
	mu.Lock()
	s.started, r.SessionRef = true, id
	mu.Unlock()

	var usage func() Usage
	if a.usage != nil {
		usage = a.usage(env, id)
	}
	var reply struct {
		StopReason string `json:"stopReason"`
		Usage      *struct {
			Input      int `json:"inputTokens"`
			Output     int `json:"outputTokens"`
			CacheRead  int `json:"cachedReadTokens"`
			CacheWrite int `json:"cachedWriteTokens"`
		} `json:"usage"`
	}
	prompt, err := c.request("session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "text", "text": t.Prompt}}})
	if err != nil {
		return fail(err)
	}
	replied := make(chan error, 1)
	go func() { replied <- c.wait(ctx, "session/prompt", prompt, &reply) }()
	inbox := t.inbox
	if a.interject == "" {
		inbox = nil // the board keeps what comes for the next turn
	}
	var interjects sync.WaitGroup
	for waiting := true; waiting; {
		select {
		case err = <-replied:
			waiting = false
		case msg := <-inbox: // it joins the turn, after the prompt on the wire
			ch, err := c.request(a.interject, map[string]any{"sessionId": id, "text": msg})
			interjects.Add(1)
			go func() {
				defer interjects.Done()
				if err != nil || c.wait(ctx, a.interject, ch, nil) != nil {
					mu.Lock()
					r.Unsent = append(r.Unsent, msg)
					mu.Unlock()
				}
			}()
		}
	}
	interjects.Wait()
	mu.Lock()
	defer mu.Unlock()
	s.end(&r, log)
	if err != nil {
		select {
		case <-c.closed:
			return fail(err)
		default: // it said no: the turn failed, the session is fine
			if ctx.Err() != nil {
				return result{}, ctx.Err()
			}
			r.Err = a.label + ": " + firstLine(strings.TrimPrefix(strings.TrimPrefix(err.Error(), "session/prompt: "), "Internal error: "))
		}
	}
	if why := stopped[reply.StopReason]; why != "" {
		r.Err = a.label + " " + why
	}
	switch {
	case s.counted: // its updates told
	case reply.Usage != nil:
		v := reply.Usage
		r.Usage.add(Usage{Input: v.Input, Output: v.Output, CacheRead: v.CacheRead, CacheWrite: v.CacheWrite})
	case usage != nil:
		r.Usage.add(usage())
	}
	return r, nil
}

// acpStream turns a turn's session updates into log lines.
type acpStream struct {
	dirs    []string // the task's folder, as spellings gives: paths under it are logged relative
	started bool     // updates before it are a resumed conversation's replay
	counted bool     // the updates carried the turn's token counts
	text    strings.Builder
	tools   map[string]*acpTool // by call id
	order   []*acpTool          // as they came
}

// acpTool is a tool call, logged once its input is known: some agents
// announce a call before its input has streamed in.
type acpTool struct {
	name, title string
	input       map[string]any
	logged      bool
}

func (s *acpStream) update(method string, params json.RawMessage, r *result, log emit) {
	if !s.started || (method != "session/update" && method != "_x.ai/session_notification") {
		return
	}
	var n struct {
		Update struct {
			Kind    string `json:"sessionUpdate"`
			Content json.RawMessage
			CallID  string         `json:"toolCallId"`
			Status  string         `json:"status"`
			Title   string         `json:"title"`
			Input   map[string]any `json:"rawInput"`
			Meta    struct {
				Tool struct {
					Name string `json:"name"`
				} `json:"x.ai/tool"`
			} `json:"_meta"`
			Usage *struct { // Grok's turn_completed: its input counts cache reads too
				Input      int     `json:"inputTokens"`
				Output     int     `json:"outputTokens"`
				CacheRead  int     `json:"cachedReadTokens"`
				CacheWrite int     `json:"cacheCreationTokens"`
				CostTicks  float64 `json:"costUsdTicks"`
			} `json:"usage"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &n) != nil {
		return
	}
	u := n.Update
	switch u.Kind {
	case "agent_message_chunk":
		var c struct {
			Text string `json:"text"`
		}
		json.Unmarshal(u.Content, &c)
		s.text.WriteString(c.Text)
	case "agent_thought_chunk", "user_message_chunk":
		s.flush(r, log)
	case "tool_call":
		s.flush(r, log)
		call := &acpTool{name: cmp.Or(u.Meta.Tool.Name, u.Title, "tool"), input: u.Input}
		s.tools[u.CallID], s.order = call, append(s.order, call)
		if u.Status != "pending" || telling(u.Input) || strings.Contains(toolName(call.name), "/") { // an MCP tool's input is not shown: no need to wait
			s.log(call, log)
		}
	case "tool_call_update":
		call := s.tools[u.CallID]
		if call == nil || call.logged {
			return
		}
		if len(u.Input) > 0 {
			call.input = u.Input
		}
		if u.Title != "" {
			call.title = u.Title
		}
		if telling(call.input) || u.Status == "completed" || u.Status == "failed" {
			s.log(call, log)
		}
	case "turn_completed":
		s.flush(r, log)
		if v := u.Usage; v != nil {
			s.counted = true
			r.Usage.add(Usage{Input: max(0, v.Input-v.CacheRead), Output: v.Output, CacheRead: v.CacheRead, CacheWrite: v.CacheWrite, CostUSD: v.CostTicks / 1e10})
		}
	}
}

// telling reports whether a call's input says what it does.
func telling(input map[string]any) bool {
	return toolLine("", input) != toolLine("", nil)
}

func (s *acpStream) log(call *acpTool, log emit) {
	call.logged = true
	name, input := call.name, call.input
	if tool, ok := input["tool_name"].(string); ok && name == "use_tool" { // Grok calls MCP tools through use_tool
		name, input = tool, nil
		if in, ok := call.input["tool_input"].(map[string]any); ok {
			input = in
		}
	}
	line := toolLine(name, input)
	if line == toolLine(name, nil) && call.title != "" && !strings.EqualFold(call.title, name) {
		line += " " + firstLine(call.title) // OpenCode titles a finished call with what it did
	}
	for _, d := range s.dirs {
		line = strings.ReplaceAll(line, d, "")
	}
	log(line)
}

// spellings are the ways an agent may write dir in a path, longest first:
// through its symlinks (macOS's /var is /private/var) or not, with or
// without the leading slash.
func spellings(dir string) []string {
	dirs := []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		dirs = []string{real, dir}
	}
	var out []string
	for _, d := range dirs {
		d = strings.TrimSuffix(d, "/") + "/"
		out = append(out, d, strings.TrimPrefix(d, "/"))
	}
	return out
}

// end logs what the turn left unlogged.
func (s *acpStream) end(r *result, log emit) {
	s.flush(r, log)
	for _, call := range s.order {
		if !call.logged {
			s.log(call, log)
		}
	}
}

func (s *acpStream) flush(r *result, log emit) {
	if t := strings.TrimSpace(s.text.String()); t != "" {
		log(t)
		r.Text = t
	}
	s.text.Reset()
}
