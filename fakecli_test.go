package main

// Antigravity and Copilot are not installed here, so the test binary plays
// them: run as `agy` or `copilot` from a PATH of scripts, it speaks the
// protocol each driver expects. This runs the
// real drivers: process launch, framing, the JSON-RPC client, resuming.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	switch os.Getenv("FAKE_CLI") {
	case "agy":
		fakeAgy()
		os.Exit(0)
	case "copilot":
		fakeCopilot()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeCLIs puts scripts named agy and copilot first on PATH, and returns
// the file each fake logs what it was asked to.
func fakeCLIs(t *testing.T) (logFile string) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range []string{"agy", "copilot"} {
		script := fmt.Sprintf("#!/bin/sh\nFAKE_CLI=%s exec %q \"$@\"\n", name, exe)
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	logFile = filepath.Join(t.TempDir(), "calls")
	t.Setenv("FAKE_LOG", logFile)
	return logFile
}

func fakeLog(format string, args ...any) {
	f, err := os.OpenFile(os.Getenv("FAKE_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, format+"\n", args...)
}

func readLog(t *testing.T, path string) []string {
	data, _ := os.ReadFile(path)
	os.Remove(path)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// fakeAgy answers one user message: it opens the conversation it was asked
// to resume, or conv-1, replies, and exits once stdin closes.
func fakeAgy() {
	fakeLog("args %s", strings.Join(os.Args[1:], " "))
	if slices.Contains(os.Args, "/usage") { // the quota probe: noise, then the JSON
		fmt.Println("Loading…")
		fmt.Println(`{"command":{"data":{"groups":[{"name":"Gemini Pro","buckets":[{"id":"pro","remaining_fraction":0.25,"reset_time":"2030-01-02T03:04:05Z"},{"id":"off","disabled":true,"remaining_fraction":0}]}]}}}`)
		return
	}
	conv := "conv-1"
	if i := slices.Index(os.Args, "--conversation"); i > 0 && i+1 < len(os.Args) {
		conv = os.Args[i+1]
	}
	in := bufio.NewReader(os.Stdin)
	line, _ := in.ReadString('\n')
	var msg struct {
		Message struct{ Content string } `json:"message"`
	}
	_ = json.Unmarshal([]byte(line), &msg)
	out := json.NewEncoder(os.Stdout)
	out.Encode(map[string]any{"event": "init", "conversation_id": conv})
	out.Encode(map[string]any{"event": "step_update", "step_update": map[string]any{
		"step_type": "agent_response", "step_index": 1, "state": "DONE",
		"text_delta": "re: " + msg.Message.Content + " in " + conv, "usage": map[string]int{"input_tokens": 7, "output_tokens": 3}}})
	out.Encode(map[string]any{"event": "result", "result": map[string]any{}})
	io.Copy(io.Discard, in)
}

// fakeCopilot serves the headless JSON-RPC: sessions s-new on create, any
// on resume but "gone", and one reply per send, ending in session.idle.
func fakeCopilot() {
	in := bufio.NewReader(os.Stdin)
	send := func(v any) {
		body, _ := json.Marshal(v)
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(body), body)
	}
	reply := func(id json.RawMessage, result any) {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	event := func(typ string, data any) {
		send(map[string]any{"jsonrpc": "2.0", "method": "session.event", "params": map[string]any{"event": map[string]any{"type": typ, "data": data}}})
	}
	for {
		body, err := readFrame(in)
		if err != nil {
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				SessionID string `json:"sessionId"`
				Prompt    string `json:"prompt"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &m)
		fakeLog("%s %s", m.Method, m.Params.SessionID)
		switch m.Method {
		case "connect":
			reply(m.ID, map[string]int{"protocolVersion": 3})
		case "session.create":
			reply(m.ID, map[string]string{"sessionId": "s-new"})
		case "session.resume":
			if m.Params.SessionID == "gone" {
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32000, "message": "session not found"}})
				continue
			}
			reply(m.ID, map[string]any{})
		case "auth.getStatus":
			reply(m.ID, map[string]any{"isAuthenticated": true, "login": "octo"})
		case "account.getQuota":
			reply(m.ID, map[string]any{"quotaSnapshots": map[string]any{
				"chat":                 map[string]any{"isUnlimitedEntitlement": true},
				"premium_interactions": map[string]any{"remainingPercentage": 70.0, "resetDate": "2030-01-01T00:00:00Z"},
			}})
		case "session.send":
			reply(m.ID, map[string]any{})
			event("assistant.message", map[string]string{"messageId": "m1", "content": "re: " + m.Params.Prompt + " in " + m.Params.SessionID})
			event("assistant.usage", map[string]int{"inputTokens": 9, "outputTokens": 2})
			event("session.idle", map[string]any{})
		default:
			reply(m.ID, map[string]any{})
		}
	}
}

func TestFakeAntigravity(t *testing.T) {
	calls := fakeCLIs(t)
	dir := t.TempDir()
	r, err := runAntigravity(context.Background(), Task{Dir: dir, Prompt: "hello"}, func(string) {})
	if err != nil || r.SessionRef != "conv-1" || r.Text != "re: hello in conv-1" || r.Usage.Output != 3 {
		t.Fatalf("first turn: %+v, %v", r, err)
	}
	if args := readLog(t, calls)[0]; strings.Contains(args, "--conversation") {
		t.Errorf("first turn args: %s", args)
	}
	r, err = runAntigravity(context.Background(), Task{Dir: dir, Prompt: "more", SessionRef: "conv-1", resume: true}, func(string) {})
	if err != nil || r.SessionRef != "conv-1" || r.Text != "re: more in conv-1" {
		t.Fatalf("reply: %+v, %v", r, err)
	}
	if args := readLog(t, calls)[0]; !strings.Contains(args, "--conversation conv-1") {
		t.Errorf("reply args: %s", args)
	}
}

func TestFakeCopilot(t *testing.T) {
	calls := fakeCLIs(t)
	dir := t.TempDir()
	run := func(task Task) (result, []string, []string) {
		t.Helper()
		lines, log := collect()
		task.Dir = dir
		r, err := runCopilot(context.Background(), task, log)
		if err != nil {
			t.Fatalf("%+v: %v", task, err)
		}
		return r, readLog(t, calls), *lines
	}
	r, methods, _ := run(Task{Prompt: "hello"})
	if r.SessionRef != "s-new" || r.Text != "re: hello in s-new" || r.Usage.Input != 9 || !slices.Contains(methods, "session.create ") {
		t.Errorf("first turn: %+v, calls %q", r, methods)
	}
	r, methods, _ = run(Task{Prompt: "more", SessionRef: "s-old", resume: true})
	if r.SessionRef != "s-old" || r.Text != "re: more in s-old" || !slices.Contains(methods, "session.resume s-old") || slices.Contains(methods, "session.create ") {
		t.Errorf("reply: %+v, calls %q", r, methods)
	}
	r, methods, lines := run(Task{Prompt: "again", SessionRef: "gone", resume: true}) // lost: a new session
	if r.SessionRef != "s-new" || !slices.Contains(methods, "session.create ") || !strings.Contains(strings.Join(lines, "\n"), "couldn't resume") {
		t.Errorf("a lost session: %+v, calls %q, log %q", r, methods, lines)
	}
}
