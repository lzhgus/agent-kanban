package main

// ── Kimi Code ───────────────────────────────────────────────────────────────
// `kimi acp`, over ACP (acp.go), in a home of the board's own: the user's
// sign-in, providers and models, none of their hooks, MCP servers or other
// settings. It runs in its "auto" mode, never asking, and is told yes when it
// asks anyway. It takes no message mid-turn ("another turn is already in
// progress"). Its token counts are in the session's wire.jsonl, a step.end
// each.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var kimi = &acpAgent{
	label: "Kimi Code",
	bin:   "kimi",
	args:  func(string) []string { return []string{"acp"} },
	env: func(Task) ([]string, error) {
		home, err := kimiHome()
		if err != nil {
			return nil, err
		}
		return append(os.Environ(), "KIMI_CODE_HOME="+home, "KIMI_CODE_NO_AUTO_UPDATE=1", "KIMI_CLI_NO_AUTO_UPDATE=1"), nil
	},
	mode:  "auto",
	allow: true,
	usage: kimiUsage,
}

func runKimi(ctx context.Context, t Task, log emit) (result, error) { return kimi.run(ctx, t, log) }

func kimiUserHome() string {
	if dir := os.Getenv("KIMI_CODE_HOME"); dir != "" {
		return dir
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".kimi-code")
}

// kimiTables are the config tables the board's Kimi home keeps: how to
// reach the models, and which.
var kimiTables = regexp.MustCompile(`^\[\s*"?(thinking|services|providers|models)\b`)

// kimiConfig keeps the user's config.toml down to its top-level settings and
// kimiTables.
func kimiConfig(toml string) string {
	var out []string
	keep := true
	for _, line := range strings.Split(toml, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "[") {
			keep = !strings.HasPrefix(t, "[[") && kimiTables.MatchString(t)
		}
		if keep {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func kimiHome() (string, error) {
	user := kimiUserHome()
	theirs, err := os.ReadFile(filepath.Join(user, "config.toml"))
	if err != nil {
		return "", errors.New("Kimi Code is not set up: run `kimi login`")
	}
	home, err := agentHome("kimi-home")
	if err != nil {
		return "", err
	}
	for _, f := range []string{"credentials", "oauth", "region", "device_id"} {
		share(filepath.Join(user, f), home)
	}
	config, mine := filepath.Join(home, "config.toml"), kimiConfig(string(theirs))
	if old, _ := os.ReadFile(config); string(old) != mine {
		if err := os.WriteFile(config, []byte(mine), 0o600); err != nil {
			return "", err
		}
	}
	return home, nil
}

// kimiUsage counts the steps a turn adds to the session's wire.jsonl.
func kimiUsage(env []string, sessionID string) func() Usage {
	home := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "KIMI_CODE_HOME="); ok {
			home = v
		}
	}
	wire := func() string {
		files, _ := filepath.Glob(filepath.Join(home, "sessions", "*", sessionID, "agents", "main", "wire.jsonl"))
		if len(files) == 0 {
			return ""
		}
		return files[0]
	}
	var from int64
	if fi, err := os.Stat(wire()); err == nil {
		from = fi.Size()
	}
	return func() (u Usage) {
		f, err := os.Open(wire())
		if err != nil {
			return u
		}
		defer f.Close()
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return u
		}
		for sc := newScanner(bufio.NewReader(f)); sc.Scan(); {
			var rec struct {
				Event struct {
					Type  string `json:"type"`
					Usage struct {
						Input      int `json:"inputOther"`
						Output     int `json:"output"`
						CacheRead  int `json:"inputCacheRead"`
						CacheWrite int `json:"inputCacheCreation"`
					} `json:"usage"`
				} `json:"event"`
			}
			if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Event.Type == "step.end" {
				v := rec.Event.Usage
				u.add(Usage{Input: v.Input, Output: v.Output, CacheRead: v.CacheRead, CacheWrite: v.CacheWrite})
			}
		}
		return u
	}
}
