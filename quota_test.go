package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

func windows(q *Quota) string {
	var s string
	for _, w := range q.Windows {
		s += fmt.Sprintf("%s=%.0f ", w.Label, w.Used)
	}
	return s
}

// The shape Claude Code 2.1.316 answers get_usage with, abridged.
func TestParseClaudeUsage(t *testing.T) {
	q, err := parseClaudeUsage(json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{
		"seven_day":{"utilization":50,"resets_at":"2030-01-01T22:00:00.2+00:00"},
		"five_hour":{"utilization":31,"resets_at":"2030-01-01T08:20:00.2+00:00"},
		"seven_day_opus":null,"tangelo":{"utilization":9},"extra_usage":{"is_enabled":false}}}`))
	if err != nil || q.Plan != "max" || windows(q) != "5h=31 Week=50 " || q.Windows[0].ResetsAt.Hour() != 8 {
		t.Errorf("%+v %v", q, err)
	}
}

func TestParseCopilotAndAgy(t *testing.T) {
	q, err := parseCopilotQuota(json.RawMessage(`{"quotaSnapshots":{"completions":{"remainingPercentage":90},"chat":{"isUnlimitedEntitlement":true},"premium_interactions":{"remainingPercentage":40,"resetDate":"2030-01-01T00:00:00Z"}}}`))
	if err != nil || windows(q) != "Premium=60 Completions=10 " {
		t.Errorf("copilot %+v %v", q, err)
	}
	// As agy 1.4 prints it: one group, a bucket per model, no reset known.
	q, err = parseAgyUsage([]byte("Signing in…\n" + `{"conversation_id":"","status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"All Models","buckets":[` +
		`{"id":"gemini-4-argon-high","name":"Gemini 4 Argon (High)","remaining_fraction":1,"reset_time":"1970-01-01T00:00:00Z"},` +
		`{"id":"claude-opus-5-5-max","name":"Claude Opus 5.5 (Max)","remaining_fraction":0.9,"reset_time":"1970-01-01T00:00:00Z"},` +
		`{"id":"gemini-3-1-pro-low","name":"Gemini 3.1 Pro (Low)","remaining_fraction":0.95,"reset_time":"1970-01-01T00:00:00Z"}]}]}}}`))
	if err != nil || windows(q) != "All Models=10 " || !q.Windows[0].ResetsAt.IsZero() {
		t.Errorf("agy %+v %v", q, err)
	}
}

// The newest rate limits Codex logged are its quota; a window past its
// reset starts over.
func TestCodexQuota(t *testing.T) {
	home := t.TempDir()
	saved := codexHome
	codexHome = func() string { return home }
	defer func() { codexHome = saved }()
	if q, err := readCodexQuota(context.Background()); q != nil || err != nil {
		t.Fatalf("no Codex: %+v %v", q, err)
	}
	dir := filepath.Join(home, "sessions", "2030", "01", "01")
	os.MkdirAll(dir, 0o755)
	future, past := time.Now().Add(3*time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
	line := func(primary, secondary string) string {
		return `{"timestamp":"2030-01-01T00:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":` + primary + `,"secondary":` + secondary + `,"plan_type":"plus"}}}` + "\n"
	}
	old := line(fmt.Sprintf(`{"used_percent":90,"window_minutes":300,"resets_at":%d}`, future), fmt.Sprintf(`{"used_percent":70,"window_minutes":10080,"resets_at":%d}`, future))
	newest := line(fmt.Sprintf(`{"used_percent":35,"window_minutes":300,"resets_at":%d}`, past), "null")
	os.WriteFile(filepath.Join(dir, "rollout-a.jsonl"), []byte(old+`{"type":"other"}`+"\n"+newest), 0o644)
	q, err := readCodexQuota(context.Background())
	if err != nil || q.Plan != "plus" || windows(q) != "5h=0 " || !q.Windows[0].ResetsAt.IsZero() {
		t.Errorf("%+v %v", q, err) // the newest line alone, its 5h reset since
	}
}

func TestUntil(t *testing.T) {
	now := time.Now()
	// Each a little inside its bucket: the clock runs on before until reads it.
	for d, want := range map[time.Duration]string{-time.Minute: "now", 30*time.Minute + 30*time.Second: "31m", 5*time.Hour + time.Minute: "5h", 50*time.Hour + 30*time.Minute: "2d2h", 48*time.Hour + time.Minute: "2d"} {
		if got := until(now.Add(d)); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
	if until(time.Time{}) != "" {
		t.Error("an unknown reset shows")
	}
}

// The probes, through the CLIs played by the test binary.
func TestQuotaProbes(t *testing.T) {
	fakeCLIs(t)
	ctx := context.Background()
	if q, err := probeCopilot(ctx); err != nil || windows(q) != "Premium=30 " {
		t.Errorf("copilot %+v %v", q, err)
	}
	if q, err := probeAntigravity(ctx); err != nil || windows(q) != "Gemini Pro=75 " {
		t.Errorf("antigravity %+v %v", q, err)
	}
}

func TestQuotaView(t *testing.T) {
	b := newBoard("")
	b.quotas = map[string]*Quota{
		"claude": {Plan: "max", At: time.Now(), Windows: []Window{{Label: "5h", Used: 31, ResetsAt: time.Now().Add(55 * time.Minute)}, {Label: "Week", Used: 90}}},
		"codex":  {Err: "not signed in"},
	}
	tt := ui.NewTester(b.view, 1440, 880)
	tt.Frame()
	for _, want := range []string{"31%", "55m", "90%", "not signed in"} {
		if !tt.HasText(want) {
			t.Errorf("%q not shown: %q", want, tt.Texts())
		}
	}
}
