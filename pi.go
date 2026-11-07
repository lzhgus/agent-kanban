package main

// ── Pi ──────────────────────────────────────────────────────────────────────
// `pi --mode rpc`: commands on stdin, events on stdout, one JSON object a
// line. A turn is a prompt; a message while it works goes in as a steer; the
// turn is over at agent_settled, when Pi will not go on by itself.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// piAgentDir is the user's Pi config folder.
func piAgentDir() string {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return dir
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".pi", "agent")
}

// piRunDir makes a config folder for one run: the user's sign-in, custom
// models and default model, none of their extensions, skills or servers,
// and the board's MCP server at url. Its sessions live in the board's Pi
// home, sessions, which outlasts it.
func piRunDir(url string) (run, sessions string, err error) {
	home, err := agentHome("pi-home")
	if err != nil {
		return "", "", err
	}
	if run, err = os.MkdirTemp(home, "run-"); err != nil {
		return "", "", err
	}
	user := piAgentDir()
	for _, f := range []string{"auth.json", "models.json", "models-store.json"} {
		share(filepath.Join(user, f), run)
	}
	var mine map[string]any
	if data, err := os.ReadFile(filepath.Join(user, "settings.json")); err == nil {
		json.Unmarshal(data, &mine)
	}
	settings := map[string]any{}
	for _, k := range []string{"defaultProvider", "defaultModel", "defaultThinkingLevel"} {
		if v, ok := mine[k]; ok {
			settings[k] = v
		}
	}
	files := map[string]any{"settings.json": settings}
	if url != "" {
		// direct: the model sees the board's tools, not codemode's script over them
		files["mcp.json"] = map[string]any{"mcpServers": map[string]any{bridgeName: map[string]any{"url": url, "exposure": "direct"}}}
	}
	for name, v := range files {
		data, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(run, name), data, 0o600); err != nil {
			os.RemoveAll(run)
			return "", "", err
		}
	}
	return run, filepath.Join(home, "sessions"), nil
}

func runPi(ctx context.Context, t Task, log emit) (result, error) {
	run, sessions, err := piRunDir(t.mcpURL)
	if err != nil {
		return result{}, err
	}
	defer os.RemoveAll(run)
	id := t.SessionRef
	if !t.resume || id == "" {
		id = newUUID()
	}
	args := []string{"--mode", "rpc", "--session-dir", sessions, "--session-id", id}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	cmd := command(ctx, t.Dir, "pi", args...)
	cmd.Env = append(os.Environ(), "PI_CODING_AGENT_DIR="+run)
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
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		for sc := newScanner(out); sc.Scan(); {
			lines <- append([]byte(nil), sc.Bytes()...)
		}
	}()
	r := result{SessionRef: id}
	send := func(cmd map[string]any) bool {
		data, _ := json.Marshal(cmd)
		_, err := in.Write(append(data, '\n'))
		return err == nil
	}
	settled := !send(map[string]any{"id": "prompt", "type": "prompt", "message": t.Prompt})
	inbox := t.inbox
	for !settled {
		select {
		case line, ok := <-lines:
			if !ok {
				settled = true
				continue
			}
			settled = piLine(line, &r, log, func(answer map[string]any) { send(answer) })
		case msg := <-inbox:
			// ponytail: a steer that lands just as Pi settles may go unanswered;
			// queue_update would tell, if that ever bites.
			if !send(map[string]any{"type": "steer", "message": msg}) {
				r.Unsent = append(r.Unsent, msg)
			}
		case <-ctx.Done():
			settled = true
		}
	}
	in.Close()  // Pi shuts down in order
	go func() { // and says nothing more worth reading
		for range lines {
		}
	}()
	if err := cmd.Wait(); ctx.Err() != nil {
		return r, ctx.Err()
	} else if err != nil && r.Err == "" && r.Text == "" {
		return r, exitErr(err, stderr)
	}
	return r, nil
}

// piLine handles one record from Pi, reporting whether the turn is over.
// answer replies to a question an extension put up: nobody is here, so no.
func piLine(line []byte, r *result, log emit, answer func(map[string]any)) bool {
	var m struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Method  string `json:"method"`
		Data    struct {
			Disposition string `json:"disposition"`
		} `json:"data"`
		Message struct {
			Role    string `json:"role"`
			Content json.RawMessage
			Usage   struct {
				Input      int `json:"input"`
				Output     int `json:"output"`
				CacheRead  int `json:"cacheRead"`
				CacheWrite int `json:"cacheWrite"`
				Cost       struct {
					Total float64 `json:"total"`
				} `json:"cost"`
			} `json:"usage"`
			StopReason   string `json:"stopReason"`
			ErrorMessage string `json:"errorMessage"`
		} `json:"message"`
		ToolName string         `json:"toolName"`
		Args     map[string]any `json:"args"`
	}
	if json.Unmarshal(line, &m) != nil {
		return false
	}
	switch m.Type {
	case "response":
		if m.ID != "prompt" {
			return false
		}
		if !m.Success {
			r.Err = "Pi: " + firstLine(m.Error)
			return true
		}
		return m.Data.Disposition == "handled" // nothing to run
	case "extension_ui_request":
		switch m.Method {
		case "select", "confirm", "input", "editor":
			answer(map[string]any{"type": "extension_ui_response", "id": m.ID, "cancelled": true})
		}
	case "tool_execution_start":
		log(toolLine(m.ToolName, m.Args))
	case "message_end":
		if m.Message.Role != "assistant" {
			return false
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		json.Unmarshal(m.Message.Content, &blocks)
		var text []string
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				text = append(text, strings.TrimSpace(b.Text))
			}
		}
		if t := strings.Join(text, "\n\n"); t != "" {
			log(t)
			r.Text = t
		}
		u := m.Message.Usage
		r.Usage.add(Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, CostUSD: u.Cost.Total})
		if m.Message.StopReason == "error" {
			r.Err = "Pi: " + firstLine(m.Message.ErrorMessage)
		} else {
			r.Err = "" // a retry that worked
		}
	case "agent_settled":
		return true
	}
	return false
}
