package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Shapes as Claude Code 2.1.316 writes them to ~/.claude/projects.
func TestWatchClaude(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "-Users-me-app")
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "s1.jsonl")
	write := func(lines ...string) {
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		f.WriteString(strings.Join(lines, ""))
		f.Close()
	}
	w := &watcher{claudeDir: root, claude: map[string]*claudeFile{}, codex: map[string]*Session{}}
	write(
		`{"type":"permission-mode","permissionMode":"default","sessionId":"s1"}`+"\n",
		`{"type":"user","cwd":"/Users/me/app","isSidechain":false,"message":{"role":"user","content":"Fix the login bug\nplease"}}`+"\n",
		`{"type":"assistant","cwd":"/Users/me/app","isSidechain":false,"message":{"id":"m1","model":"claude-opus-5-5","stop_reason":"tool_use","content":[{"type":"thinking","thinking":""}],"usage":{"input_tokens":3,"output_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":50}}}`+"\n",
		`{"type":"assistant","cwd":"/Users/me/app","isSidechain":false,"message":{"id":"m1","model":"claude-opus-5-5","stop_reason":"tool_use","content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}],"usage":{"input_tokens":3,"output_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":50}}}`+"\n",
		`{"type":"assistant","isSidechain":true,"message":{"id":"sub1","stop_reason":"end_turn","content":[{"type":"text","text":"subagent"}],"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n",
		`{"type":"user","cwd":"/Users/me/app","isSidechain":false,"message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`+"\n",
		`{"type":"assistant","cwd":"/Users/me/app","isSidechain":false,"message":{"id":"m2"`, // still being written
	)
	now := time.Now()
	got := w.scan(now)
	if len(got) != 1 {
		t.Fatalf("%d sessions", len(got))
	}
	s := got[0]
	if s.Agent != "claude" || s.ID != "s1" || s.Title != "Fix the login bug" || s.Dir != "/Users/me/app" || !s.Working {
		t.Errorf("session %+v", s)
	}
	if s.Usage != (Usage{Input: 4, Output: 11, CacheRead: 100, CacheWrite: 50}) || s.Tokens != 165 { // m1 once, plus the subagent
		t.Errorf("usage %+v, tokens %d", s.Usage, s.Tokens)
	}
	if strings.Join(s.Recent, "|") != "› Fix the login bug|▸ Bash go test ./..." {
		t.Errorf("recent %q", s.Recent)
	}
	// The partial line completes, the turn ends, and Claude names the session.
	write(
		`,"model":"claude-opus-5-5","stop_reason":"end_turn","content":[{"type":"text","text":"Fixed."}],"usage":{"input_tokens":5,"output_tokens":20}}}`+"\n",
		`{"type":"ai-title","aiTitle":"Login bug","sessionId":"s1"}`+"\n",
	)
	s = w.scan(now)[0]
	if s.Working || s.Title != "Login bug" || s.Recent[len(s.Recent)-1] != "Fixed." || s.Usage.Output != 31 {
		t.Errorf("after the turn: %+v", s)
	}
	// Stale or old sessions are not running, or not shown.
	if w.scan(now.Add(20 * time.Minute))[0].Working {
		t.Error("a stale turn still works")
	}
	if len(w.scan(now.Add(25*time.Hour))) != 0 {
		t.Error("an old session is shown")
	}
}

// Codex's schema, the columns the watcher reads, in a database of our own.
func TestWatchCodex(t *testing.T) {
	w := newWatcher()
	if w.sqlite == "" {
		t.Skip("no sqlite3")
	}
	w.codexDir, w.claudeDir = t.TempDir(), t.TempDir()
	now := time.Now()
	ms := now.Add(-time.Minute).UnixMilli()
	run := func(db, sql string) {
		out, err := exec.Command(w.sqlite, filepath.Join(w.codexDir, db), sql).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", db, err, out)
		}
	}
	run("state_5.sqlite", `create table threads (id text, cwd text, title text, name text, first_user_message text,
		tokens_used int, model text, updated_at int, updated_at_ms int, archived int, source text);`+
		`insert into threads values ('a1', '/x/app', 'Refactor auth', null, 'refactor', 5000, 'gpt-5.6', 0, `+fmtInt(ms)+`, 0, 'cli');`+
		`insert into threads values ('a2', '/x/app', 'Old', null, '', 1, '', 0, `+fmtInt(ms-int64(48*time.Hour/time.Millisecond))+`, 0, 'cli');`+
		`insert into threads values ('a3', '/x/app', 'Sub', null, '', 1, '', 0, `+fmtInt(ms)+`, 0, '{"subagent":{}}');`)
	run("thread_history_1.sqlite", `create table thread_turns (thread_id text, turn_id text, rollout_ordinal int, status text);`+
		`create table thread_items (thread_id text, turn_id text, item_id text, rollout_ordinal int, item_type text, item_json text);`+
		`insert into thread_turns values ('a1', 't1', 1, 'completed'), ('a1', 't2', 5, 'inProgress');`+
		`insert into thread_items values ('a1','t2','i1',6,'userMessage','{"content":[{"type":"text","text":"now the tests"}]}'),`+
		`('a1','t2','i2',7,'commandExecution','{"command":"go test ./..."}'),`+
		`('a1','t2','i3',8,'reasoning','{}'),`+
		`('a1','t2','i4',9,'agentMessage','{"text":"Running them."}');`)
	got := w.scan(now)
	if len(got) != 1 {
		t.Fatalf("sessions %+v", got)
	}
	s := got[0]
	if s.Agent != "codex" || s.Title != "Refactor auth" || s.Tokens != 5000 || !s.Working || s.Model != "gpt-5.6" {
		t.Errorf("session %+v", s)
	}
	if strings.Join(s.Recent, "|") != "› now the tests|▸ Bash go test ./...|Running them." {
		t.Errorf("recent %q", s.Recent)
	}
	run("thread_history_1.sqlite", `update thread_turns set status = 'failed' where turn_id = 't2';`)
	run("state_5.sqlite", `update threads set updated_at_ms = updated_at_ms + 1 where id = 'a1';`)
	if s = w.scan(now)[0]; s.Working || s.Failed == "" {
		t.Errorf("after failing: %+v", s)
	}
}

func fmtInt(n int64) string { return strconv.FormatInt(n, 10) }
