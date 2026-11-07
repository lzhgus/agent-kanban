package main

// ── OpenCode ────────────────────────────────────────────────────────────────
// `opencode acp --pure`, over ACP (acp.go), without the user's plugins and
// with a config of the board's own on top of theirs: every tool allowed, its
// own question tool off (it asks through the board's ask_user), no sharing,
// snapshots or updates, and their MCP servers off. Their providers and
// default model still apply. The prompt's reply carries the turn's token
// counts. It takes no message mid-turn.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

var opencode = &acpAgent{
	label: "OpenCode",
	bin:   "opencode",
	args:  func(string) []string { return []string{"acp", "--pure"} },
	env:   func(Task) ([]string, error) { return opencodeEnv() },
}

func runOpencode(ctx context.Context, t Task, log emit) (result, error) {
	return opencode.run(ctx, t, log)
}

// opencodeUserDir is where OpenCode keeps the user's config, or data.
func opencodeUserDir(xdg, fallback string) string {
	if dir := os.Getenv(xdg); dir != "" {
		return filepath.Join(dir, "opencode")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, fallback, "opencode")
}

func opencodeEnv() ([]string, error) {
	home, err := agentHome("opencode-home")
	if err != nil {
		return nil, err
	}
	config := map[string]any{
		"$schema": "https://opencode.ai/config.json", "autoupdate": false, "share": "disabled", "snapshot": false,
		"permission": map[string]string{"*": "allow", "question": "deny"},
	}
	off := map[string]any{}
	for _, name := range []string{"opencode.json", "opencode.jsonc"} {
		data, err := os.ReadFile(filepath.Join(opencodeUserDir("XDG_CONFIG_HOME", ".config"), name))
		if err != nil {
			continue
		}
		var theirs struct {
			MCP map[string]any `json:"mcp"`
		}
		// ponytail: JSON only; a .jsonc with comments keeps its servers on.
		if json.Unmarshal(data, &theirs) == nil {
			for server := range theirs.MCP {
				off[server] = map[string]bool{"enabled": false}
			}
		}
	}
	if len(off) > 0 {
		config["mcp"] = off
	}
	data, _ := json.MarshalIndent(config, "", "  ")
	file := filepath.Join(home, "opencode.json")
	if old, _ := os.ReadFile(file); !bytes.Equal(old, data) {
		if err := os.WriteFile(file, data, 0o600); err != nil {
			return nil, err
		}
	}
	env := []string{}
	for _, kv := range os.Environ() { // a config of the user's own would come before ours
		if !strings.HasPrefix(kv, "OPENCODE_CONFIG=") && !strings.HasPrefix(kv, "OPENCODE_CONFIG_CONTENT=") {
			env = append(env, kv)
		}
	}
	env = append(env, "OPENCODE_CONFIG_DIR="+home, "OPENCODE_DISABLE_AUTOUPDATE=1")
	// Its sessions go in the board's data folder, with the user's sign-in
	// linked in. OpenCode 2 keeps sign-ins in its database instead: there it
	// runs in the user's data folder.
	if auth := filepath.Join(opencodeUserDir("XDG_DATA_HOME", filepath.Join(".local", "share")), "auth.json"); fileExists(auth) {
		data := filepath.Join(home, "data")
		if err := os.MkdirAll(filepath.Join(data, "opencode"), 0o700); err != nil {
			return nil, err
		}
		share(auth, filepath.Join(data, "opencode"))
		env = append(env, "XDG_DATA_HOME="+data)
	}
	return env, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
