package main

// Watching the agents you run outside the board, read-only: the sessions
// Claude Code writes to ~/.claude/projects/*/<id>.jsonl, the threads Codex
// keeps in ~/.codex/state_5.sqlite and thread_history_1.sqlite, the
// conversations Antigravity keeps in ~/.gemini/antigravity-cli, and the
// sessions Grok Build keeps in ~/.grok/sessions/<cwd>/<id>.
// Nothing here starts, stops or writes to them.

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	recentWindow = 24 * time.Hour   // sessions active in this window are shown
	staleAfter   = 15 * time.Minute // a turn with no activity for this long is not running
	idleDone     = 2 * time.Hour    // an ended session idle this long is done, not waiting for you
	recentLines  = 40
	scanEvery    = 3 * time.Second
)

// Session is an agent conversation started outside the board.
type Session struct {
	Agent   string // "claude", "codex", "antigravity" or "grok"
	ID      string
	Title   string
	Dir     string
	Model   string
	Updated time.Time
	Working bool // a turn is in progress
	Failed  string
	Usage   Usage // Claude Code: per kind; Codex reports a total only
	Tokens  int   // every token, input and output
	Recent  []string
}

func (s *Session) key() string { return s.Agent + ":" + s.ID }

type watcher struct {
	claudeDir string
	codexDir  string
	agyDir    string
	grokDir   string
	sqlite    string
	claude    map[string]*claudeFile // by path
	codex     map[string]*Session    // by thread id, with its Recent kept between scans
	grok      map[string]*grokFile   // by session folder
	lastErr   string                 // the last error logged
}

func newWatcher() *watcher {
	home, _ := os.UserHomeDir()
	w := &watcher{
		claudeDir: filepath.Join(home, ".claude", "projects"),
		codexDir:  filepath.Join(home, ".codex"),
		agyDir:    filepath.Join(home, ".gemini", "antigravity-cli"),
		grokDir:   filepath.Join(cmp.Or(os.Getenv("GROK_HOME"), filepath.Join(home, ".grok")), "sessions"),
		claude:    map[string]*claudeFile{},
		codex:     map[string]*Session{},
		grok:      map[string]*grokFile{},
	}
	// The system's sqlite3 first: others on PATH (Android's) may lack -json.
	if _, err := os.Stat("/usr/bin/sqlite3"); err == nil {
		w.sqlite = "/usr/bin/sqlite3"
	} else if p, err := exec.LookPath("sqlite3"); err == nil {
		w.sqlite = p
	}
	return w
}

// scan returns the sessions active within the window, newest first.
func (w *watcher) scan(now time.Time) []*Session {
	var out []*Session
	out = append(out, w.scanClaude(now)...)
	codex, err := w.scanCodex(now)
	out = append(out, codex...)
	agy, agyErr := w.scanAgy(now)
	out = append(out, agy...)
	out = append(out, w.scanGrok(now)...)
	err = errors.Join(err, agyErr)
	if msg := fmt.Sprint(err); err != nil && msg != w.lastErr { // once, not every scan
		log.Printf("watching: %v", err)
		w.lastErr = msg
	} else if err == nil {
		w.lastErr = ""
	}
	slices.SortFunc(out, func(a, b *Session) int { return b.Updated.Compare(a.Updated) })
	return out
}

// ── Claude Code ─────────────────────────────────────────────────────────────

type claudeFile struct {
	offset  int64
	s       Session
	prompt  string           // the first prompt, the title until Claude names the session
	usage   map[string]Usage // by API message id: a message is logged once per content block
	working bool
}

func (w *watcher) scanClaude(now time.Time) []*Session {
	paths, _ := filepath.Glob(filepath.Join(w.claudeDir, "*", "*.jsonl"))
	var out []*Session
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil || now.Sub(fi.ModTime()) > recentWindow {
			continue
		}
		f := w.claude[p]
		if f == nil {
			f = &claudeFile{usage: map[string]Usage{}}
			f.s.Agent, f.s.ID = "claude", strings.TrimSuffix(filepath.Base(p), ".jsonl")
			w.claude[p] = f
		}
		if fi.Size() < f.offset { // rewritten: start over
			*f = claudeFile{usage: map[string]Usage{}, s: Session{Agent: "claude", ID: f.s.ID}}
		}
		if fi.Size() > f.offset {
			if err := f.read(p); err != nil {
				log.Printf("reading %s: %v", p, err)
			}
		}
		if f.s.Dir == "" { // no conversation yet, only metadata
			continue
		}
		s := f.s
		s.Updated = fi.ModTime()
		s.Working = f.working && now.Sub(s.Updated) < staleAfter
		if s.Title == "" {
			s.Title = firstLine(f.prompt)
		}
		if s.Title == "" { // only slash commands so far
			s.Title = "Claude Code in " + filepath.Base(s.Dir)
		}
		s.Recent = slices.Clone(f.s.Recent)
		out = append(out, &s)
	}
	return out
}

// read consumes the lines appended since the last read; a line still being
// written waits for the next.
func (f *claudeFile) read(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(file, 256*1024)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil { // io.EOF: a partial line, read again next time
			break
		}
		f.offset += int64(len(line))
		f.line(line)
	}
	f.s.Usage = Usage{}
	for _, u := range f.usage {
		f.s.Usage.add(u)
	}
	u := f.s.Usage
	f.s.Tokens = u.Input + u.CacheRead + u.CacheWrite + u.Output
	return nil
}

func (f *claudeFile) line(line []byte) {
	var e struct {
		Type        string `json:"type"`
		AiTitle     string `json:"aiTitle"`
		Summary     string `json:"summary"`
		Cwd         string `json:"cwd"`
		IsSidechain bool   `json:"isSidechain"`
		Message     struct {
			ID         string          `json:"id"`
			Model      string          `json:"model"`
			StopReason string          `json:"stop_reason"`
			Content    json.RawMessage `json:"content"`
			Usage      *struct {
				Input      int `json:"input_tokens"`
				Output     int `json:"output_tokens"`
				CacheRead  int `json:"cache_read_input_tokens"`
				CacheWrite int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil {
		return
	}
	m := e.Message
	switch e.Type {
	case "ai-title":
		f.s.Title = e.AiTitle
	case "summary":
		if f.s.Title == "" {
			f.s.Title = e.Summary
		}
	case "assistant":
		if u := m.Usage; u != nil && m.ID != "" { // subagents' calls count too
			f.usage[m.ID] = Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
		}
		if e.IsSidechain {
			return
		}
		f.s.Dir, f.s.Model = e.Cwd, m.Model
		var blocks []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		}
		_ = json.Unmarshal(m.Content, &blocks)
		for _, b := range blocks {
			switch b.Type {
			case "text":
				f.recent(b.Text)
			case "tool_use":
				f.recent(toolLine(b.Name, b.Input))
			}
		}
		f.working = m.StopReason != "end_turn" && m.StopReason != "stop_sequence"
	case "user":
		if e.IsSidechain {
			return
		}
		f.s.Dir = e.Cwd
		var text string
		if json.Unmarshal(m.Content, &text) == nil { // a prompt typed by the user
			if strings.HasPrefix(text, "<") { // a slash command's or hook's wrapper, not a prompt
				return
			}
			if f.prompt == "" {
				f.prompt = text
			}
			f.recent("› " + firstLine(text))
			f.working = true
			return
		}
		f.working = !bytes.Contains(m.Content, []byte("[Request interrupted by user")) // else a tool's result: the turn goes on
	}
}

func (f *claudeFile) recent(line string) {
	if line = strings.TrimSpace(line); line != "" {
		f.s.Recent = appendRecent(f.s.Recent, line)
	}
}

func appendRecent(lines []string, line string) []string {
	lines = append(lines, line)
	if n := len(lines) - recentLines; n > 0 {
		lines = slices.Delete(lines, 0, n)
	}
	return lines
}

// ── Codex ───────────────────────────────────────────────────────────────────

var threadID = regexp.MustCompile(`^[0-9a-fA-F-]{1,64}$`)

// query runs SQL against an agent's database, read-only, and decodes the
// rows. The agent keeps writing while it runs; WAL lets us read meanwhile.
func (w *watcher) query(path, sql string, rows any) error {
	if _, err := os.Stat(path); err != nil {
		return nil // the agent is not here
	}
	if w.sqlite == "" {
		return fmt.Errorf("sqlite3 not found")
	}
	uri := "file:" + path + "?mode=ro"
	// A WAL database whose agent has closed it has no -wal: a read-only
	// connection cannot create one and fails to open (14). Nothing writes
	// then: read the file as it is.
	if _, err := os.Stat(path + "-wal"); err != nil && walMode(path) {
		uri += "&immutable=1"
	}
	out, err := exec.Command(w.sqlite, "-readonly", "-json", uri, sql).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("%s: %s", filepath.Base(path), strings.TrimSpace(string(ee.Stderr)))
		}
		return err
	}
	if len(bytes.TrimSpace(out)) == 0 { // no rows
		return nil
	}
	return json.Unmarshal(out, rows)
}

// walMode reads the database header: versions 2 at bytes 18 and 19 are WAL.
func walMode(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := make([]byte, 20)
	_, err = io.ReadFull(f, h)
	return err == nil && h[18] == 2
}

func (w *watcher) scanCodex(now time.Time) ([]*Session, error) {
	var threads []struct {
		ID      string `json:"id"`
		Cwd     string `json:"cwd"`
		Title   string `json:"title"`
		Tokens  int    `json:"tokens_used"`
		Model   string `json:"model"`
		Updated int64  `json:"updated"`
	}
	since := now.Add(-recentWindow).UnixMilli()
	// Subagents' threads (a JSON source) belong to their parent's.
	err := w.query(filepath.Join(w.codexDir, "state_5.sqlite"), fmt.Sprintf(`
		select id, cwd, coalesce(nullif(name, ''), nullif(title, ''), first_user_message) as title,
			tokens_used, coalesce(model, '') as model, coalesce(updated_at_ms, updated_at * 1000) as updated
		from threads
		where archived = 0 and source not like '{%%' and coalesce(updated_at_ms, updated_at * 1000) >= %d
		order by updated desc limit 100`, since), &threads)
	if err != nil {
		return nil, err
	}
	var changed []string
	seen := map[string]bool{}
	for _, t := range threads {
		if !threadID.MatchString(t.ID) {
			continue
		}
		seen[t.ID] = true
		s := w.codex[t.ID]
		updated := time.UnixMilli(t.Updated)
		if s == nil || !s.Updated.Equal(updated) {
			changed = append(changed, t.ID)
		}
		if s == nil {
			s = &Session{Agent: "codex", ID: t.ID}
			w.codex[t.ID] = s
		}
		s.Title, s.Dir, s.Model, s.Tokens, s.Updated = firstLine(t.Title), t.Cwd, t.Model, t.Tokens, updated
		if s.Title == "" {
			s.Title = "Codex in " + filepath.Base(t.Cwd)
		}
	}
	for id := range w.codex {
		if !seen[id] {
			delete(w.codex, id)
		}
	}
	if len(changed) > 0 {
		if err := w.codexDetails(changed); err != nil {
			return nil, err
		}
	}
	var out []*Session
	for _, t := range threads {
		if s := w.codex[t.ID]; s != nil {
			c := *s
			c.Working = s.Working && now.Sub(s.Updated) < staleAfter
			c.Recent = slices.Clone(s.Recent)
			out = append(out, &c)
		}
	}
	return out, nil
}

// codexDetails reads the threads' last turn status and recent items.
func (w *watcher) codexDetails(ids []string) error {
	in := "'" + strings.Join(ids, "','") + "'" // ids matched threadID: no quotes in them
	var rows []struct {
		Thread string `json:"thread_id"`
		Kind   string `json:"kind"`
		Text   string `json:"text"`
	}
	err := w.query(filepath.Join(w.codexDir, "thread_history_1.sqlite"), `
		select thread_id, 'status' as kind, status as text, 1000000000 as ord from thread_turns t
		where thread_id in (`+in+`)
			and rollout_ordinal = (select max(rollout_ordinal) from thread_turns where thread_id = t.thread_id)
		union all
		select thread_id, item_type, case item_type
				when 'agentMessage' then substr(json_extract(item_json, '$.text'), 1, 800)
				when 'userMessage' then substr(json_extract(item_json, '$.content[0].text'), 1, 300)
				when 'commandExecution' then substr(json_extract(item_json, '$.command'), 1, 300)
				when 'fileChange' then json_extract(item_json, '$.changes[0].path')
				when 'mcpToolCall' then json_extract(item_json, '$.server') || '/' || json_extract(item_json, '$.tool')
			end, ord
		from (
			select thread_id, item_type, item_json, rollout_ordinal as ord,
				row_number() over (partition by thread_id order by rollout_ordinal desc) as rn
			from thread_items
			where thread_id in (`+in+`)
				and item_type in ('agentMessage', 'userMessage', 'commandExecution', 'fileChange', 'mcpToolCall')
		) where rn <= `+fmt.Sprint(recentLines)+`
		order by thread_id, ord`, &rows)
	if err != nil {
		return err
	}
	for _, id := range ids {
		w.codex[id].Recent, w.codex[id].Working, w.codex[id].Failed = nil, false, ""
	}
	for _, r := range rows {
		s := w.codex[r.Thread]
		if s == nil || strings.TrimSpace(r.Text) == "" {
			continue
		}
		switch r.Kind {
		case "status":
			s.Working = r.Text == "inProgress"
			if r.Text == "failed" {
				s.Failed = "the last turn failed"
			}
		case "agentMessage":
			s.Recent = appendRecent(s.Recent, strings.TrimSpace(r.Text))
		case "userMessage":
			s.Recent = appendRecent(s.Recent, "› "+firstLine(r.Text))
		case "commandExecution":
			s.Recent = appendRecent(s.Recent, "▸ Bash "+firstLine(r.Text))
		case "fileChange":
			s.Recent = appendRecent(s.Recent, "▸ Edit "+r.Text)
		case "mcpToolCall":
			s.Recent = appendRecent(s.Recent, "▸ "+r.Text)
		}
	}
	return nil
}

// ── Antigravity ─────────────────────────────────────────────────────────────
// agy keeps a summary of each conversation in conversation_summaries.db, and
// the prompts you typed, with their conversation, in history.jsonl.

const agyTime = "2006-01-02 15:04:05.999999999-07:00"

func (w *watcher) scanAgy(now time.Time) ([]*Session, error) {
	var rows []struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		Preview  string `json:"preview"`
		Dirs     string `json:"dirs"`
		Modified string `json:"modified"`
		Busy     int    `json:"busy"`
	}
	// Subagents' conversations belong to their parent's.
	err := w.query(filepath.Join(w.agyDir, "conversation_summaries.db"), `
		select conversation_id as id, title, preview, workspace_uris as dirs, last_modified_time as modified,
			cast(not_fully_idle as int) * (1 - cast(killed as int)) as busy
		from conversation_summaries
		where parent_conversation_id = ''
		order by last_modified_time desc limit 100`, &rows)
	if err != nil {
		return nil, err
	}
	prompts := w.agyPrompts()
	var out []*Session
	for _, r := range rows {
		updated, err := time.Parse(agyTime, r.Modified)
		if err != nil || now.Sub(updated) > recentWindow {
			continue
		}
		s := &Session{Agent: "antigravity", ID: r.ID, Updated: updated, Title: firstLine(cmp.Or(r.Title, r.Preview))}
		s.Working = r.Busy == 1 && now.Sub(updated) < staleAfter
		var dirs []string
		if json.Unmarshal([]byte(r.Dirs), &dirs) == nil && len(dirs) > 0 {
			if u, err := url.Parse(dirs[0]); err == nil {
				s.Dir = u.Path
			}
		}
		for _, p := range prompts[r.ID] {
			s.Recent = appendRecent(s.Recent, "› "+firstLine(p.Display))
			s.Dir = cmp.Or(s.Dir, p.Workspace)
		}
		if s.Title == "" && len(prompts[r.ID]) > 0 {
			s.Title = firstLine(prompts[r.ID][0].Display)
		}
		s.Title = cmp.Or(s.Title, "Antigravity in "+filepath.Base(s.Dir))
		out = append(out, s)
	}
	return out, nil
}

type agyPrompt struct {
	Display      string `json:"display"`
	Workspace    string `json:"workspace"`
	Conversation string `json:"conversationId"`
}

// agyPrompts reads history.jsonl, by conversation, oldest first.
// ponytail: reads the whole file each scan; it is a few KB of prompts.
func (w *watcher) agyPrompts() map[string][]agyPrompt {
	data, _ := os.ReadFile(filepath.Join(w.agyDir, "history.jsonl"))
	by := map[string][]agyPrompt{}
	for line := range bytes.Lines(data) {
		var p agyPrompt
		if json.Unmarshal(line, &p) == nil && p.Conversation != "" && strings.TrimSpace(p.Display) != "" {
			by[p.Conversation] = append(by[p.Conversation], p)
		}
	}
	return by
}

// ── Grok Build ──────────────────────────────────────────────────────────────
// Each session is a folder: summary.json names it, updates.jsonl is its
// conversation as ACP session updates, usage.json its token counts. The
// board's own Grok tasks run in a home of their own, not here.

type grokFile struct {
	offset  int64
	stream  acpStream
	user    strings.Builder // the user's message streaming in
	working bool
	recent  []string
}

func (w *watcher) scanGrok(now time.Time) []*Session {
	paths, _ := filepath.Glob(filepath.Join(w.grokDir, "*", "*", "updates.jsonl"))
	var out []*Session
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil || now.Sub(fi.ModTime()) > recentWindow {
			continue
		}
		dir := filepath.Dir(p)
		var sum struct {
			Info struct {
				ID  string `json:"id"`
				Cwd string `json:"cwd"`
			} `json:"info"`
			Title   string `json:"generated_title"`
			Summary string `json:"session_summary"`
			Model   string `json:"current_model_id"`
		}
		if data, err := os.ReadFile(filepath.Join(dir, "summary.json")); err != nil || json.Unmarshal(data, &sum) != nil || sum.Info.Cwd == "" {
			continue
		}
		f := w.grok[dir]
		if f == nil || fi.Size() < f.offset { // new, or rewritten (a rewind): start over
			f = &grokFile{stream: acpStream{started: true, dirs: spellings(sum.Info.Cwd), tools: map[string]*acpTool{}}}
			w.grok[dir] = f
		}
		if fi.Size() > f.offset {
			if err := f.read(p); err != nil {
				log.Printf("reading %s: %v", p, err)
			}
		}
		s := &Session{Agent: "grok", ID: cmp.Or(sum.Info.ID, filepath.Base(dir)), Dir: sum.Info.Cwd, Model: sum.Model, Updated: fi.ModTime(),
			Title: firstLine(cmp.Or(sum.Title, sum.Summary)), Recent: slices.Clone(f.recent)}
		s.Working = f.working && now.Sub(s.Updated) < staleAfter
		if s.Title == "" {
			s.Title = "Grok Build in " + filepath.Base(s.Dir)
		}
		var usage struct {
			Session struct {
				Input      int     `json:"inputTokens"` // cache reads included
				Output     int     `json:"outputTokens"`
				CacheRead  int     `json:"cachedReadTokens"`
				CacheWrite int     `json:"cacheCreationTokens"`
				CostTicks  float64 `json:"costUsdTicks"`
			} `json:"session"`
		}
		if data, err := os.ReadFile(filepath.Join(dir, "usage.json")); err == nil && json.Unmarshal(data, &usage) == nil {
			u := usage.Session
			s.Usage = Usage{Input: max(0, u.Input-u.CacheRead), Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, CostUSD: u.CostTicks / 1e10}
			s.Tokens = s.Usage.total()
		}
		out = append(out, s)
	}
	return out
}

// read consumes the updates appended since the last read; a line still
// being written waits for the next.
func (f *grokFile) read(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(file, 256*1024)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break
		}
		f.offset += int64(len(line))
		f.line(line)
	}
	return nil
}

func (f *grokFile) line(line []byte) {
	var e struct {
		Params struct {
			Update struct {
				Kind    string `json:"sessionUpdate"`
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &e) != nil {
		return
	}
	u := e.Params.Update
	if u.Kind == "user_message_chunk" {
		f.user.WriteString(u.Content.Text)
		f.working = true
		return
	}
	if f.user.Len() > 0 { // the message is whole
		f.say("› " + firstLine(grokTyped(f.user.String())))
		f.user.Reset()
	}
	switch u.Kind {
	case "agent_message_chunk", "agent_thought_chunk", "tool_call":
		f.working = true
	case "turn_completed":
		f.working = false
	}
	var params struct {
		Params json.RawMessage `json:"params"`
	}
	json.Unmarshal(line, &params)
	var r result
	f.stream.update("session/update", params.Params, &r, f.say)
	if u.Kind == "turn_completed" {
		f.stream.end(&r, f.say)
		f.stream.tools, f.stream.order = map[string]*acpTool{}, nil
	}
}

func (f *grokFile) say(line string) {
	if line = strings.TrimSpace(line); line != "" {
		f.recent = appendRecent(f.recent, line)
	}
}

// grokTyped is what the user typed, out of the wrapper Grok puts round a
// message sent while it works.
func grokTyped(msg string) string {
	if _, rest, ok := strings.Cut(msg, "<user_query>"); ok {
		if typed, _, ok := strings.Cut(rest, "</user_query>"); ok {
			return strings.TrimSpace(typed)
		}
	}
	return msg
}
