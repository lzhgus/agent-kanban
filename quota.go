package main

// Subscription quotas: how much of each agent's plan windows (Claude's five
// hours and week, Copilot's premium requests, Antigravity's weekly limits,
// Grok's weekly credits, Codex's) is used, and when each resets. Claude,
// Copilot, Antigravity and Grok are asked through their CLIs, which costs no model call; Codex is read
// from the rate limits it logs on every turn.

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Window is one quota window of a plan.
type Window struct {
	Label    string
	Used     float64   // percent, 0-100
	ResetsAt time.Time // zero if unknown
}

// Quota is an agent's plan, as last read.
type Quota struct {
	Plan    string
	Windows []Window
	Err     string
	At      time.Time // when the figures are from
}

const (
	quotaEvery = 5 * time.Minute
	quotaAfter = time.Minute // after a turn, a refresh at most this often
	probeLimit = 30 * time.Second
)

var quotaProbes = map[string]func(ctx context.Context) (*Quota, error){
	"claude":      probeClaude,
	"copilot":     probeCopilot,
	"antigravity": probeAntigravity,
	"grok":        probeGrok,
	"codex":       readCodexQuota,
}

// probeQuotas reads every agent whose CLI is here; an agent not installed
// is left out.
func probeQuotas() map[string]*Quota {
	out := map[string]*Quota{}
	for name, probe := range quotaProbes {
		bin := agentBins[name]
		if _, err := exec.LookPath(bin); bin != "" && err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), probeLimit)
		q, err := probe(ctx)
		cancel()
		switch {
		case err != nil:
			out[name] = &Quota{Err: firstLine(err.Error()), At: time.Now()}
		case q != nil:
			out[name] = q
		}
	}
	return out
}

func pct(v float64) float64 { return min(max(v, 0), 100) }

// ── Claude Code ─────────────────────────────────────────────────────────────
// The SDK's control protocol over stream-json: initialize, then get_usage.

var claudeWindows = map[string]string{"five_hour": "5h", "seven_day": "Week", "seven_day_opus": "Opus week", "seven_day_sonnet": "Sonnet week", "seven_day_oauth_apps": "Apps week"}

func probeClaude(ctx context.Context) (*Quota, error) {
	cmd := command(ctx, os.TempDir(), "claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--setting-sources", "")
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }()
	for i, sub := range []string{"initialize", "get_usage"} {
		fmt.Fprintf(in, `{"type":"control_request","request_id":"q%d","request":{"subtype":%q}}`+"\n", i, sub)
	}
	for sc := newScanner(out); sc.Scan(); {
		var m struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Error     string          `json:"error"`
				Response  json.RawMessage `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.Type != "control_response" || m.Response.RequestID != "q1" {
			continue
		}
		if m.Response.Subtype == "error" {
			return nil, errors.New("Claude Code: " + m.Response.Error)
		}
		return parseClaudeUsage(m.Response.Response)
	}
	return nil, errors.New("Claude Code did not report its usage")
}

func parseClaudeUsage(raw json.RawMessage) (*Quota, error) {
	var u struct {
		Subscription string                     `json:"subscription_type"`
		Limits       map[string]json.RawMessage `json:"rate_limits"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, err
	}
	q := &Quota{Plan: u.Subscription, At: time.Now()}
	var keys []string
	for k := range u.Limits {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { // the known windows first, in their order
		oi, oj := windowOrder(keys[i]), windowOrder(keys[j])
		if oi != oj {
			return oi < oj
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		label, known := claudeWindows[k]
		if !known {
			continue // the response lists many experimental windows, null for most plans
		}
		var w struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		}
		if json.Unmarshal(u.Limits[k], &w) != nil || w.Utilization == nil {
			continue
		}
		reset, _ := time.Parse(time.RFC3339Nano, w.ResetsAt)
		q.Windows = append(q.Windows, Window{Label: label, Used: pct(*w.Utilization), ResetsAt: reset})
	}
	return q, nil
}

func windowOrder(k string) int {
	for i, o := range []string{"five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet", "seven_day_oauth_apps"} {
		if o == k {
			return i
		}
	}
	return 99
}

// ── GitHub Copilot ──────────────────────────────────────────────────────────

var copilotQuotaLabels = map[string]string{"premium_interactions": "Premium", "chat": "Chat", "completions": "Completions"}

func probeCopilot(ctx context.Context) (*Quota, error) {
	cmd := command(ctx, os.TempDir(), "copilot", "--headless", "--no-auto-update", "--stdio")
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }()
	c := newRPC(in, out, func(string, json.RawMessage) {})
	if err := c.call(ctx, "connect", map[string]any{}, nil); err != nil {
		return nil, err
	}
	var auth struct {
		IsAuthenticated bool `json:"isAuthenticated"`
	}
	if err := c.call(ctx, "auth.getStatus", map[string]any{}, &auth); err != nil {
		return nil, err
	}
	if !auth.IsAuthenticated {
		return nil, errors.New("not signed in: run copilot login")
	}
	var raw json.RawMessage
	if err := c.call(ctx, "account.getQuota", map[string]any{}, &raw); err != nil {
		return nil, err
	}
	return parseCopilotQuota(raw)
}

func parseCopilotQuota(raw json.RawMessage) (*Quota, error) {
	var r struct {
		Snapshots map[string]*struct {
			Remaining *float64 `json:"remainingPercentage"`
			Unlimited bool     `json:"isUnlimitedEntitlement"`
			Reset     string   `json:"resetDate"`
		} `json:"quotaSnapshots"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	q := &Quota{At: time.Now()}
	var keys []string
	for k := range r.Snapshots {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { // premium requests first
		return keys[i] == "premium_interactions" || (keys[j] != "premium_interactions" && keys[i] < keys[j])
	})
	for _, k := range keys {
		s := r.Snapshots[k]
		if s == nil || s.Unlimited || s.Remaining == nil {
			continue
		}
		label := copilotQuotaLabels[k]
		if label == "" {
			label = strings.ReplaceAll(k, "_", " ")
		}
		reset, _ := time.Parse(time.RFC3339, s.Reset)
		q.Windows = append(q.Windows, Window{Label: label, Used: pct(100 - *s.Remaining), ResetsAt: reset})
	}
	return q, nil
}

// ── Grok Build ──────────────────────────────────────────────────────────────
// Its ACP server tells the plan (_x.ai/auth/check_subscription) and how much
// of the week's credits it used (_x.ai/billing).

func probeGrok(ctx context.Context) (*Quota, error) {
	home, err := grokHome()
	if err != nil {
		return nil, err
	}
	c, stop, stderr, err := grok.start(ctx, os.TempDir(), "", grokEnv(home), func(string, json.RawMessage) {})
	if err != nil {
		return nil, err
	}
	defer stop()
	var sub, billing json.RawMessage
	for _, call := range []struct {
		method string
		out    *json.RawMessage
	}{{"initialize", nil}, {"_x.ai/auth/check_subscription", &sub}, {"_x.ai/billing", &billing}} {
		var params any = map[string]any{}
		if call.method == "initialize" {
			params = acpInitialize
		}
		var out any
		if call.out != nil {
			out = call.out
		}
		if err := c.call(ctx, call.method, params, out); err != nil {
			return nil, exitErr(err, stderr)
		}
	}
	return parseGrokBilling(sub, billing)
}

func parseGrokBilling(sub, billing json.RawMessage) (*Quota, error) {
	var s struct {
		Authenticated bool `json:"authenticated"`
		Meta          struct {
			Tier string `json:"subscription_tier"`
		} `json:"meta"`
	}
	var b struct {
		Tier   string `json:"subscription_tier"`
		Config struct {
			Credits *float64 `json:"creditUsagePercent"`
			Period  struct {
				End string `json:"end"`
			} `json:"currentPeriod"`
			Cap struct {
				Val float64 `json:"val"`
			} `json:"onDemandCap"`
			Used struct {
				Val float64 `json:"val"`
			} `json:"onDemandUsed"`
		} `json:"config"`
	}
	if err := json.Unmarshal(sub, &s); err != nil {
		return nil, err
	}
	if !s.Authenticated {
		return nil, errors.New("not signed in: run grok login")
	}
	json.Unmarshal(billing, &b)
	q := &Quota{Plan: cmp.Or(b.Tier, s.Meta.Tier), At: time.Now()}
	end, _ := time.Parse(time.RFC3339, b.Config.Period.End)
	if c := b.Config.Credits; c != nil {
		q.Windows = append(q.Windows, Window{Label: "Week", Used: pct(*c), ResetsAt: end})
	}
	if b.Config.Cap.Val > 0 {
		q.Windows = append(q.Windows, Window{Label: "On demand", Used: pct(b.Config.Used.Val / b.Config.Cap.Val * 100), ResetsAt: end})
	}
	return q, nil
}

// ── Antigravity ─────────────────────────────────────────────────────────────

func probeAntigravity(ctx context.Context) (*Quota, error) {
	cmd := command(ctx, os.TempDir(), "agy", "-p", "/usage", "--output-format", "json")
	stderr := &stderrTail{}
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, exitErr(err, stderr)
	}
	return parseAgyUsage(out)
}

// parseAgyUsage reads the JSON object agy prints, among whatever else.
func parseAgyUsage(out []byte) (*Quota, error) {
	start := strings.IndexByte(string(out), '{')
	if start < 0 {
		return nil, errors.New("Antigravity printed no usage")
	}
	var r struct {
		Command struct {
			Data struct {
				Groups []struct {
					Name    string `json:"name"`
					Buckets []struct {
						ID        string   `json:"id"`
						Disabled  bool     `json:"disabled"`
						Remaining *float64 `json:"remaining_fraction"`
						Reset     string   `json:"reset_time"`
					} `json:"buckets"`
				} `json:"groups"`
			} `json:"data"`
		} `json:"command"`
	}
	if err := json.NewDecoder(strings.NewReader(string(out[start:]))).Decode(&r); err != nil {
		return nil, err
	}
	// A group's buckets, one per model, share pools: like agy, show the one
	// closest to its limit. A reset agy does not know is the Unix epoch.
	q := &Quota{At: time.Now()}
	for _, g := range r.Command.Data.Groups {
		var w *Window
		for _, bk := range g.Buckets {
			if bk.ID == "" || bk.Disabled || bk.Remaining == nil {
				continue
			}
			used := pct((1 - *bk.Remaining) * 100)
			if w == nil || used > w.Used {
				reset, _ := time.Parse(time.RFC3339, bk.Reset)
				if reset.Unix() <= 0 {
					reset = time.Time{}
				}
				w = &Window{Label: cmp.Or(g.Name, bk.ID), Used: used, ResetsAt: reset}
			}
		}
		if w != nil {
			q.Windows = append(q.Windows, *w)
		}
	}
	return q, nil
}

// ── Codex ───────────────────────────────────────────────────────────────────
// Every turn logs the account's rate limits in its rollout: the newest one
// is Codex's quota as of then. A window that has reset since starts over.

var codexHome = func() string { h, _ := os.UserHomeDir(); return filepath.Join(h, ".codex") }

func readCodexQuota(context.Context) (*Quota, error) {
	var newest string
	var newestAt time.Time
	filepath.WalkDir(filepath.Join(codexHome(), "sessions"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
			if fi, err := d.Info(); err == nil && fi.ModTime().After(newestAt) {
				newest, newestAt = p, fi.ModTime()
			}
		}
		return nil
	})
	if newest == "" {
		return nil, nil // no Codex here
	}
	f, err := os.Open(newest)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, _ := f.Stat(); fi.Size() > 4<<20 { // the end holds the latest
		f.Seek(-4<<20, io.SeekEnd)
	}
	type limit struct {
		Used    float64 `json:"used_percent"`
		Minutes int     `json:"window_minutes"`
		Resets  int64   `json:"resets_at"`
	}
	type entry struct {
		Timestamp time.Time `json:"timestamp"`
		Payload   struct {
			Limits *struct {
				Primary, Secondary *limit
				Plan               string `json:"plan_type"`
			} `json:"rate_limits"`
		} `json:"payload"`
	}
	var found entry
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if strings.Contains(string(line), `"rate_limits"`) {
			var e entry // each its own: the decoder fills pointers it finds
			if json.Unmarshal(line, &e) == nil && e.Payload.Limits != nil {
				found = e
			}
		}
		if err != nil {
			break
		}
	}
	l := found.Payload.Limits
	if l == nil {
		return nil, nil
	}
	q := &Quota{Plan: l.Plan, At: found.Timestamp}
	for _, w := range []*limit{l.Primary, l.Secondary} {
		if w == nil {
			continue
		}
		label := "Week"
		if w.Minutes < 24*60 {
			label = fmt.Sprintf("%dh", w.Minutes/60)
		}
		used, reset := pct(w.Used), time.Unix(w.Resets, 0)
		if w.Resets > 0 && time.Now().After(reset) {
			used, reset = 0, time.Time{} // reset since: a new window
		}
		q.Windows = append(q.Windows, Window{Label: label, Used: used, ResetsAt: reset})
	}
	return q, nil
}

// until is a reset countdown: 42m, 3h, 2d5h.
func until(t time.Time) string {
	d := time.Until(t)
	switch {
	case t.IsZero():
		return ""
	case d <= 0:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes())+1)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	days := int(d.Hours()) / 24
	if h := int(d.Hours()) % 24; h > 0 {
		return fmt.Sprintf("%dd%dh", days, h)
	}
	return fmt.Sprintf("%dd", days)
}
