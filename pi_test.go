package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakePi serves `pi --mode rpc`: per prompt, an extension's question (which
// must be declined), a tool call, the answer and agent_settled. A prompt
// "slow…" waits for a steer and answers both; "broken" ends in an error.
// It logs its args and the config folder it was given.
func fakePi() {
	dir := os.Getenv("PI_CODING_AGENT_DIR")
	mcp, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	settings, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	link, _ := os.Readlink(filepath.Join(dir, "auth.json"))
	fakeLog("args %s", strings.Join(os.Args[1:], " "))
	fakeLog("config mcp=%s settings=%s auth=%s", mcp, settings, link)
	in := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	type cmd struct {
		ID, Type, Message string
		Cancelled         bool
	}
	var held []cmd // read while waiting for another type: Pi takes records in any order
	next := func(typ string) (c cmd, ok bool) {
		for i, h := range held {
			if typ == "" || h.Type == typ {
				held = slices.Delete(held, i, i+1)
				return h, true
			}
		}
		for in.Scan() {
			c = cmd{}
			json.Unmarshal(in.Bytes(), &c)
			if typ == "" || c.Type == typ {
				return c, true
			}
			held = append(held, c)
		}
		return c, false
	}
	for {
		c, ok := next("")
		if !ok {
			return
		}
		if c.Type != "prompt" {
			continue
		}
		out.Encode(map[string]any{"id": c.ID, "type": "response", "command": "prompt", "success": true, "data": map[string]string{"disposition": "started"}})
		out.Encode(map[string]any{"type": "extension_ui_request", "id": "ui-1", "method": "notify", "message": "hello"})
		out.Encode(map[string]any{"type": "extension_ui_request", "id": "ui-2", "method": "confirm", "title": "Allow?"})
		answer, _ := next("extension_ui_response")
		fakeLog("ui %s cancelled=%v", answer.ID, answer.Cancelled)
		out.Encode(map[string]any{"type": "tool_execution_start", "toolCallId": "t1", "toolName": "bash", "args": map[string]string{"command": "ls"}})
		text := c.Message
		if strings.HasPrefix(text, "slow") {
			s, _ := next("steer")
			fakeLog("%s %s", s.Type, s.Message)
			text += " + " + s.Message
		}
		msg := map[string]any{"role": "assistant", "content": []map[string]string{{"type": "thinking", "thinking": "hm"}, {"type": "text", "text": "re: " + text}},
			"usage": map[string]any{"input": 10, "output": 3, "cacheRead": 4, "cacheWrite": 1, "cost": map[string]float64{"total": 0.02}}, "stopReason": "stop"}
		if text == "broken" {
			msg["stopReason"], msg["errorMessage"], msg["content"] = "error", "401 unauthorized", []any{}
		}
		out.Encode(map[string]any{"type": "message_end", "message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": c.Message}}}})
		out.Encode(map[string]any{"type": "message_end", "message": msg})
		out.Encode(map[string]any{"type": "agent_end", "messages": []any{}})
		out.Encode(map[string]any{"type": "agent_settled", "aborted": false})
	}
}

func TestFakePi(t *testing.T) {
	calls := fakeCLIs(t)
	user := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", user)
	os.WriteFile(filepath.Join(user, "auth.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(user, "settings.json"), []byte(`{"defaultProvider":"kimi-coding","defaultModel":"k3","packages":["npm:some-extension"],"theme":"dark"}`), 0o600)
	dir := t.TempDir()
	run := func(task Task) (result, []string, string) {
		t.Helper()
		lines, log := collect()
		task.Dir = dir
		r, err := runPi(context.Background(), task, log)
		if err != nil {
			t.Fatalf("%+v: %v", task, err)
		}
		return r, readLog(t, calls), strings.Join(*lines, "\n")
	}
	r, got, lines := run(Task{Prompt: "hello", mcpURL: "http://127.0.0.1:1/mcp/x"})
	if len(r.SessionRef) != 36 || r.Text != "re: hello" || r.Err != "" || r.Usage != (Usage{Input: 10, Output: 3, CacheRead: 4, CacheWrite: 1, CostUSD: 0.02}) {
		t.Errorf("first turn: %+v", r)
	}
	sessions := filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "agent-kanban", "pi-home", "sessions")
	want := []string{
		"args --mode rpc --session-dir " + sessions + " --session-id " + r.SessionRef,
		`config mcp={"mcpServers":{"kanban":{"exposure":"direct","url":"http://127.0.0.1:1/mcp/x"}}} settings={"defaultModel":"k3","defaultProvider":"kimi-coding"} auth=` + filepath.Join(user, "auth.json"),
		"ui ui-2 cancelled=true",
	}
	if !slices.Equal(got, want) {
		t.Errorf("calls\n%q\nwant\n%q", got, want)
	}
	if lines != "▸ Bash ls\nre: hello" {
		t.Errorf("log %q", lines)
	}
	if runs, _ := filepath.Glob(filepath.Join(filepath.Dir(sessions), "run-*")); len(runs) != 0 {
		t.Errorf("left %q", runs)
	}

	first := r.SessionRef
	r, got, _ = run(Task{Prompt: "more", SessionRef: first, resume: true, Model: "openai/gpt-5"})
	if r.SessionRef != first || !strings.HasSuffix(got[0], "--session-id "+first+" --model openai/gpt-5") || strings.Contains(got[1], "mcp={") {
		t.Errorf("reply: %+v, calls %q", r, got)
	}
	r, _, _ = run(Task{Prompt: "broken"})
	if r.Err != "Pi: 401 unauthorized" || r.SessionRef == first {
		t.Errorf("an error: %+v", r)
	}

	inbox := make(chan string, 1)
	inbox <- "use tabs"
	r, got, _ = run(Task{Prompt: "slow work", inbox: inbox})
	if r.Text != "re: slow work + use tabs" || len(r.Unsent) != 0 || !slices.Contains(got, "steer use tabs") {
		t.Errorf("live: %+v, calls %q", r, got)
	}
}
