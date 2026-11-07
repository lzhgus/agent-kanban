package main

// ── Grok Build ──────────────────────────────────────────────────────────────
// `grok agent stdio`, over ACP (acp.go). Its own _x.ai/session_notification
// carries each turn's token counts and cost, and _x.ai/interject hands it a
// message mid-turn.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// grokConfig is the agent's own Grok home config: the user's sign-in is
// shared, nothing else of theirs (rules, skills, MCP servers, hooks, memory)
// is.
const grokConfig = `# Written by Agent Kanban. Only auth.json is shared with your own ~/.grok.
[cli]
auto_update = false
use_leader = false

[memory]
enabled = false

[compat.claude]
skills = false
rules = false
agents = false
mcps = false
hooks = false
sessions = false

[compat.cursor]
skills = false
rules = false
agents = false
mcps = false
hooks = false
sessions = false

[compat.codex]
sessions = false
`

// grokHome readies the board's Grok home and returns it.
// ponytail: the user's custom [model.*] tables are not carried over; copy
// them in when someone runs Grok Build on their own models.
func grokHome() (string, error) {
	home, err := agentHome("grok-home")
	if err != nil {
		return "", err
	}
	config := filepath.Join(home, "config.toml")
	if old, _ := os.ReadFile(config); string(old) != grokConfig {
		if err := os.WriteFile(config, []byte(grokConfig), 0o600); err != nil {
			return "", err
		}
	}
	user := os.Getenv("GROK_HOME")
	if user == "" {
		h, _ := os.UserHomeDir()
		user = filepath.Join(h, ".grok")
	}
	if !share(filepath.Join(user, "auth.json"), home) && os.Getenv("XAI_API_KEY") == "" {
		return "", errors.New("Grok Build is not signed in: run `grok login`")
	}
	return home, nil
}

// grokEnv runs Grok in its home, with the user's Claude Code and Cursor
// setup (which Grok would load too) and its memory turned off.
func grokEnv(home string) []string {
	env := append(os.Environ(), "GROK_HOME="+home, "GROK_DISABLE_AUTOUPDATER=1", "GROK_MEMORY=0")
	for _, vendor := range []string{"CLAUDE", "CURSOR"} {
		for _, what := range []string{"SKILLS", "RULES", "AGENTS", "MCPS", "HOOKS"} {
			env = append(env, "GROK_"+vendor+"_"+what+"_ENABLED=0")
		}
	}
	return env
}

// trustFolder lets Grok work in dir unasked.
func trustFolder(home, dir string) error {
	file := filepath.Join(home, "trusted_folders.toml")
	have, _ := os.ReadFile(file)
	paths := []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		paths = append(paths, real)
	}
	var add strings.Builder
	for _, p := range paths {
		header := fmt.Sprintf("[folders.%q]", p)
		if !strings.Contains(string(have), header) {
			fmt.Fprintf(&add, "%s\ntrusted = true\ndecided_at = %d\n\n", header, time.Now().Unix())
		}
	}
	if add.Len() == 0 {
		return nil
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(add.String())
	return err
}

var grok = &acpAgent{
	label: "Grok Build",
	bin:   "grok",
	args: func(model string) []string {
		args := []string{"agent", "--no-leader", "--always-approve"}
		if model != "" {
			args = append(args, "-m", model)
		}
		return append(args, "stdio")
	},
	modelInArg: true,
	env: func(t Task) ([]string, error) {
		home, err := grokHome()
		if err != nil {
			return nil, err
		}
		if err := trustFolder(home, t.Dir); err != nil {
			return nil, err
		}
		return grokEnv(home), nil
	},
	meta:      map[string]any{"yoloMode": true},
	interject: "_x.ai/interject",
}

func runGrok(ctx context.Context, t Task, log emit) (result, error) { return grok.run(ctx, t, log) }
