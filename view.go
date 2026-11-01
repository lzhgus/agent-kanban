package main

// The board's look: sage-tinted paper over a
// translucent sidebar, floating rounded panes that part by background steps
// and soft shadows rather than outlines, one lime accent, and monospace for
// what machines say. Running work breathes: its status dot pulses.

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
)

// palette holds the board's colors, light and dark.
type palette struct {
	window, pane, column, card, pill, stroke ui.Color
	ink, muted, brand, lime, amber, red, sky ui.Color
}

var (
	lightPalette = palette{
		window: ui.Hex("#f4f5f1"), pane: ui.Hex("#ffffff"), column: ui.Hex("#f4f5f1"), card: ui.Hex("#ffffff"),
		pill: ui.Hex("#eef0ea"), stroke: ui.Hex("#e4e6df"),
		ink: ui.Hex("#13160f"), muted: ui.Hex("#75707d"), brand: ui.Hex("#4f7d14"), lime: ui.Hex("#c3ec74"),
		amber: ui.Hex("#b07812"), red: ui.Hex("#d64545"), sky: ui.Hex("#2b7bbf"),
	}
	darkPalette = palette{
		window: ui.Hex("#111310"), pane: ui.Hex("#141713"), column: ui.Hex("#191c17"), card: ui.Hex("#20241e"),
		pill: ui.Hex("#262b23"), stroke: ui.Hex("#2a2f27"),
		ink: ui.Hex("#f4f6f0"), muted: ui.Hex("#9a95a2"), brand: ui.Hex("#c3ec74"), lime: ui.Hex("#c3ec74"),
		amber: ui.Hex("#f5b84a"), red: ui.Hex("#ff6b6b"), sky: ui.Hex("#7cc7ff"),
	}
)

// theme gives the widgets (inputs, selects, scroll bars) the palette.
func (p palette) theme(base *ui.Theme) *ui.Theme {
	t := *base
	t.Background, t.Surface, t.SurfaceHover, t.SurfacePressed = p.pane, p.pill, p.pill.Mix(p.ink, 0.06), p.pill.Mix(p.ink, 0.1)
	t.Border, t.Text, t.TextMuted = p.stroke, p.ink, p.muted
	t.Accent, t.AccentHover, t.AccentPressed, t.AccentText = p.lime, p.lime.Mix(p.ink, 0.06), p.lime.Mix(p.ink, 0.12), ui.Hex("#13160f")
	t.Danger, t.Warning, t.Success = p.red, p.amber, p.brand
	t.Selection, t.Focus = p.lime.Alpha(0.45), p.brand
	t.Radius = 8
	return &t
}

func (p palette) agent(name string) ui.Color {
	dark := p.ink == darkPalette.ink
	switch name {
	case "claude":
		return pick(dark, "#e8956b", "#c96442")
	case "codex":
		return p.sky
	case "antigravity":
		return pick(dark, "#c3ec74", "#5f8f1f")
	case "copilot":
		return pick(dark, "#b9a3e3", "#7d5fc9")
	}
	return p.muted
}

func pick(dark bool, d, l string) ui.Color {
	if dark {
		return ui.Hex(d)
	}
	return ui.Hex(l)
}

func (p palette) status(s Status) ui.Color {
	switch s {
	case Running:
		return p.brand
	case Review:
		return p.amber
	case Done:
		return p.muted
	case Failed:
		return p.red
	}
	return p.sky
}

var statusHint = map[Status]string{Todo: "not started", Running: "working", Review: "needs you", Done: "finished", Failed: "stopped on an error"}

// Icons from Lucide (ISC), drawn in the text's color.
func lucide(paths string) *ui.SVG {
	return ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">` + paths + `</svg>`))
}

var (
	iconPlus   = lucide(`<path d="M5 12h14"/><path d="M12 5v14"/>`)
	iconX      = lucide(`<path d="M18 6 6 18"/><path d="m6 6 12 12"/>`)
	iconPlay   = lucide(`<polygon points="6 3 20 12 6 21 6 3"/>`)
	iconStop   = lucide(`<rect width="14" height="14" x="5" y="5" rx="2"/>`)
	iconTrash  = lucide(`<path d="M3 6h18"/><path d="M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6"/><path d="M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2"/>`)
	iconArrow  = lucide(`<path d="M5 12h14"/><path d="m12 5 7 7-7 7"/>`)
	iconEye    = lucide(`<path d="M2.062 12.348a1 1 0 0 1 0-.696 10.75 10.75 0 0 1 19.876 0 1 1 0 0 1 0 .696 10.75 10.75 0 0 1-19.876 0"/><circle cx="12" cy="12" r="3"/>`)
	iconFolder = lucide(`<path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/>`)
	iconKanban = lucide(`<path d="M6 5v11"/><path d="M12 5v6"/><path d="M18 5v14"/>`)
	iconMerge  = lucide(`<circle cx="18" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><path d="M6 21V9a9 9 0 0 0 9 9"/>`)
	iconBranch = lucide(`<line x1="6" x2="6" y1="3" y2="15"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M18 9a9 9 0 0 1-9 9"/>`)
)

const (
	titleBarHeight = 52
	sidebarWidth   = 236
	detailWidth    = 400
	changesWidth   = 680 // the detail pane while it shows a diff
	paneRadius     = 12
	controlRadius  = 8 // every button and field: one radius
	pulse          = 900 * time.Millisecond
)

// ── frame ───────────────────────────────────────────────────────────────────

func (b *board) view(c *ui.Context) {
	p := lightPalette
	if c.Theme().Dark {
		p = darkPalette
	}
	c.SetTheme(p.theme(c.Theme()))
	vibrant := c.Vibrancy()
	if vibrant {
		c.Root().Background(ui.Transparent)
	} else {
		c.Root().Background(p.window)
	}
	ui.Row(c).Fill().AlignItems(ui.Stretch).Children(func() {
		b.sidebarView(c, p)
		ui.Row(c).Grow(1).MinWidth(0).Gap(8).Padding(8, 8, 8, 0).AlignItems(ui.Stretch).Children(func() {
			b.boardPane(c, p)
			if b.selected != nil {
				b.detailView(c, p, b.selected)
			} else if s := b.findSession(b.selectedSession); s != nil {
				b.sessionDetailView(c, p, s)
			}
		})
	})
}

// pane is a floating panel: background steps and a soft shadow, no outline.
func pane(e ui.Element, p palette) ui.Element {
	return e.Radius(paneRadius).Background(p.pane).
		Shadow(0, 1, 2, 0, p.ink.Alpha(0.05)).Shadow(0, 8, 24, -8, p.ink.Alpha(0.08))
}

// ── sidebar ─────────────────────────────────────────────────────────────────

func (b *board) sidebarView(c *ui.Context, p palette) {
	ui.Column(c).Width(sidebarWidth).Shrink(0).Padding(0, 12, 14, 12).Gap(4).Children(func() {
		ui.Box(c).Height(titleBarHeight - 8).DragWindow() // under the traffic lights
		ui.Row(c).Gap(8).AlignItems(ui.Center).Padding(0, 4, 10, 4).Children(func() {
			ui.Box(c).Size(24, 24).Radius(7).Background(p.lime).Center().Children(func() {
				ui.Icon(c, iconKanban).Size(14, 14).TextColor(ui.Hex("#13160f"))
			})
			ui.Text(c, "Agent Kanban").FontSize(14).Bold()
		})
		if button(c, p, "new-task", "New task", iconPlus, primary).FillWidth().Clicked() {
			b.composing = !b.composing
		}
		ui.Text(c, "AGENTS").FontSize(10.5).Bold().LetterSpacing(0.8).TextColor(p.muted).Padding(18, 6, 6, 6)
		b.agentRow(c, p, "", "All agents")
		for _, name := range []string{"claude", "codex", "antigravity", "copilot"} {
			b.agentRow(c, p, name, name)
		}
		ui.Box(c).Grow(1)
		ui.Row(c).Gap(8).AlignItems(ui.Center).Padding(6).Children(func() {
			statusDot(c, p.brand, true)
			ui.Text(c, "Watching Claude Code & Codex").FontSize(11).TextColor(p.muted)
		})
		ui.Text(c, "Outside sessions idle 2 h move to Done.").FontSize(11).TextColor(p.muted).Wrap().Padding(0, 6)
	})
}

// agentRow filters the board to one agent and shows what it spent: the
// board's runs, and the sessions watched in the last 24 hours.
func (b *board) agentRow(c *ui.Context, p palette, name, label string) {
	var u Usage
	n, total := 0, 0
	for _, t := range b.Tasks {
		if name == "" || t.Agent == name {
			n++
			u.add(t.Usage)
			total += t.Usage.Input + t.Usage.CacheRead + t.Usage.CacheWrite + t.Usage.Output
		}
	}
	for _, s := range b.sessions {
		if name == "" || s.Agent == name {
			n++
			total += s.Tokens
		}
	}
	row := ui.ButtonBase(c.Key("agent-"+label)).Label(label).FillWidth().Padding(7, 8).Radius(controlRadius).Column().Gap(2).AlignItems(ui.Stretch).Justify(ui.Start)
	switch {
	case b.filter == name:
		row.Background(p.pane.Alpha(0.75)).Shadow(0, 1, 2, 0, p.ink.Alpha(0.06))
	case row.Hovered():
		row.Background(p.ink.Alpha(0.05))
	}
	if row.Clicked() {
		b.filter = name
	}
	row.Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if name == "" {
				ui.Icon(c, iconKanban).Size(12, 12).TextColor(p.muted)
			} else {
				ui.Box(c).Size(8, 8).Radius(4).Background(p.agent(name)).Margin(0, 2)
			}
			ui.Text(c, label).FontSize(13).FontWeight(500).Grow(1)
			ui.Text(c, fmt.Sprint(n)).FontSize(11).TextColor(p.muted).FontFeatures("tnum")
		})
		ui.Text(c, tokens(total)+" tokens"+cost(u.CostUSD)).FontSize(11).Font("monospace").TextColor(p.muted).Padding(0, 0, 0, 18)
	})
}

// ── board ───────────────────────────────────────────────────────────────────

func (b *board) visible(agent string) bool { return b.filter == "" || b.filter == agent }

func (b *board) boardPane(c *ui.Context, p palette) {
	pane(ui.Column(c).Grow(1).MinWidth(0), p).Children(func() {
		running, waiting := 0, 0
		for _, t := range b.Tasks {
			if b.visible(t.Agent) && t.Status == Running {
				running++
			} else if b.visible(t.Agent) && t.Status == Review {
				waiting++
			}
		}
		for _, s := range b.sessions {
			switch st := b.sessionStatus(s); {
			case !b.visible(s.Agent):
			case st == Running:
				running++
			case st == Review:
				waiting++
			}
		}
		title := "All agents"
		if b.filter != "" {
			title = b.filter
		}
		ui.Row(c).Height(titleBarHeight).Shrink(0).Padding(0, 16).Gap(10).AlignItems(ui.Center).DragWindow().Children(func() {
			ui.Text(c, title).FontSize(15).Bold()
			ui.Text(c, fmt.Sprintf("%d running · %d need you", running, waiting)).FontSize(12).TextColor(p.muted)
		})
		if b.composing {
			b.composerView(c, p)
		}
		type col struct {
			status   Status
			tasks    []*Task
			sessions []*Session
		}
		var cols []col
		for _, s := range columns {
			if tasks, sessions := b.items(s); s != Failed || len(tasks)+len(sessions) > 0 { // Failed only when something failed
				cols = append(cols, col{s, tasks, sessions})
			}
		}
		// The columns share the pane's width, down to a minimum; past it,
		// as beside a diff, they scroll sideways.
		w, _ := c.Size()
		avail := w - sidebarWidth - 8 - 24 - float32(10*(len(cols)-1))
		if b.selected != nil || b.selectedSession != "" {
			avail -= b.detailWidth() + 8
		}
		width := max(150, avail/float32(len(cols)))
		ui.ScrollHorizontal(c.Key("columns")).Grow(1).MinHeight(0).Gap(10).Padding(4, 12, 12, 12).AlignItems(ui.Stretch).Children(func() {
			for _, k := range cols {
				b.columnView(c, p, k.status, k.tasks, k.sessions, width)
			}
		})
	})
}

func (b *board) composerView(c *ui.Context, p palette) {
	ui.Column(c.Key("composer")).Margin(0, 12, 8, 12).Padding(12).Gap(10).Radius(10).Background(p.column).Children(func() {
		ui.TextArea(c.Key("prompt"), &b.prompt).Placeholder("Describe the task. The agent works in the folder below.").Label("Prompt").Height(72).Radius(controlRadius).Background(p.pane)
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Select(c.Key("agent"), &b.agent, agentNames).Label("Agent").Width(130)
			ui.TextInput(c.Key("model"), &b.model).Placeholder("Model (default)").Label("Model").Width(140)
			ui.Row(c).Grow(1).MinWidth(0).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, iconFolder).Size(14, 14).TextColor(p.muted)
				ui.TextInput(c.Key("dir"), &b.dir).Placeholder("Working directory").Label("Working directory").Grow(1).Font("monospace").FontSize(12)
			})
			ui.TextInput(c.Key("title"), &b.title).Placeholder("Title (optional)").Label("Title").Width(160)
		})
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "Runs headless with permissions bypassed: pick a folder you trust it with.").FontSize(11).TextColor(p.muted).Grow(1)
			if button(c, p, "cancel", "Cancel", nil, ghost).Clicked() {
				b.composing = false
			}
			empty := strings.TrimSpace(b.prompt) == ""
			if buttonIf(c, p, "add", "Add", nil, secondary, empty).Clicked() {
				b.add(false)
				b.composing = false
			}
			if buttonIf(c, p, "add-run", "Add & Run", iconPlay, primary, empty).Clicked() {
				b.add(true)
				b.composing = false
			}
		})
	})
}

// items returns what a column shows, under the agent filter.
func (b *board) items(s Status) (tasks []*Task, sessions []*Session) {
	for _, t := range b.Tasks {
		if t.Status == s && b.visible(t.Agent) {
			tasks = append(tasks, t)
		}
	}
	for _, sess := range b.sessions {
		if b.sessionStatus(sess) == s && b.visible(sess.Agent) {
			sessions = append(sessions, sess)
		}
	}
	return tasks, sessions
}

func (b *board) columnView(c *ui.Context, p palette, s Status, tasks []*Task, sessions []*Session, width float32) {
	col := ui.Column(c.Key(s)).Width(width).Shrink(0).Padding(10, 8, 8, 8).Gap(8).Radius(10).Background(p.column)
	over := false
	if task, ok := ui.DragOver[*Task](col); ok && task.Status != s {
		over = true
	}
	if sess, ok := ui.DragOver[*Session](col); ok && (s == Done || s == Review) && b.sessionStatus(sess) != s {
		over = true
	}
	if over {
		col.Background(p.lime.Alpha(0.16)).Border(1, p.brand.Alpha(0.5)).BorderStyle(ui.BorderDashed)
	}
	if task, ok := ui.Drop[*Task](col); ok {
		b.move(task, s)
	}
	if sess, ok := ui.Drop[*Session](col); ok {
		b.moveSession(sess, s)
	}
	col.Children(func() {
		ui.Row(c).Gap(7).AlignItems(ui.Center).Padding(0, 4, 2, 4).Tooltip(statusHint[s]).Children(func() {
			statusDot(c, p.status(s), false)
			ui.Text(c, string(s)).FontSize(12.5).Bold().MaxLines(1)
			ui.Text(c, fmt.Sprint(len(tasks)+len(sessions))).FontSize(11.5).TextColor(p.muted).FontFeatures("tnum")
		})
		ui.Scroll(c).Grow(1).Gap(8).Padding(2).Children(func() {
			for _, t := range tasks {
				b.cardView(c, p, t)
			}
			for _, sess := range sessions {
				b.sessionCardView(c, p, sess)
			}
			if len(tasks)+len(sessions) == 0 {
				ui.Text(c, "Nothing here").FontSize(11.5).TextColor(p.muted.Alpha(0.7)).Padding(14, 6)
			}
		})
	})
}

// card is the shared face of a task and a session: it lifts under the
// pointer and gives a little under a press.
func card(e ui.Element, p palette, selected bool) ui.Element {
	e.Padding(11, 12).Gap(6).Radius(10).Background(p.card).
		Shadow(0, 1, 2, 0, p.ink.Alpha(0.06)).InsetShadow(0, 0, 0, 1, p.stroke.Alpha(0.7))
	pose := ui.Pose{}
	switch {
	case e.Pressed():
		pose = ui.Pose{Scale: 0.98}
	case e.Hovered():
		pose = ui.Pose{Y: -2}
		e.Shadow(0, 6, 16, -4, p.ink.Alpha(0.14))
	}
	e.AnimatePose(pose, ui.Snappy)
	if selected {
		e.InsetShadow(0, 0, 0, 2, p.brand)
	}
	if e.Dragging() {
		e.Opacity(0.35)
	}
	return e
}

func agentLabel(c *ui.Context, p palette, name string, running bool) {
	ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
		if running {
			statusDot(c, p.brand, true)
		} else {
			ui.Box(c).Size(7, 7).Radius(3.5).Background(p.agent(name))
		}
		ui.Text(c, name).FontSize(11.5).FontWeight(500).TextColor(p.agent(name))
	})
}

func (b *board) cardView(c *ui.Context, p palette, task *Task) {
	e := card(ui.Column(c.Key(task)).Label(task.Title), p, b.selected == task)
	if task.Status != Running {
		e.Drag(task)
	}
	if e.Clicked() {
		b.selected, b.selectedSession = task, ""
	}
	e.Children(func() {
		ui.Row(c).AlignItems(ui.Center).Children(func() {
			agentLabel(c, p, task.Agent, task.Status == Running)
			ui.Box(c).Grow(1)
			ui.Text(c, fmt.Sprintf("#%d", task.ID)).FontSize(11).Font("monospace").TextColor(p.muted)
		})
		ui.Text(c, task.Title).FontSize(13).FontWeight(600).Wrap().MaxLines(2)
		meta := filepath.Base(task.Dir)
		if total := task.Usage.Input + task.Usage.CacheRead + task.Usage.CacheWrite + task.Usage.Output; total > 0 {
			meta += " · " + tokens(total) + " tok" + cost(task.Usage.CostUSD)
		}
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, meta).FontSize(11).Font("monospace").TextColor(p.muted).MaxLines(1).Shrink(1).MinWidth(0)
			if ch := task.Changes; ch.Files > 0 {
				diffStat(c, p, ch.Added, ch.Deleted)
			}
		})
		if task.Err != "" {
			ui.Text(c, task.Err).FontSize(11).TextColor(p.red).Wrap().MaxLines(2)
		}
	})
}

func (b *board) sessionCardView(c *ui.Context, p palette, s *Session) {
	e := card(ui.Column(c.Key(s.key())).Label(s.Title), p, b.selectedSession == s.key())
	if !s.Working {
		e.Drag(s)
	}
	if e.Clicked() {
		b.selected, b.selectedSession = nil, s.key()
	}
	e.Children(func() {
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			agentLabel(c, p, s.Agent, s.Working)
			ui.Box(c).Grow(1)
			ui.Text(c, ago(s.Updated)).FontSize(11).TextColor(p.muted).MaxLines(1).FontFeatures("tnum")
		})
		ui.Text(c, s.Title).FontSize(13).FontWeight(600).Wrap().MaxLines(2)
		ui.Row(c).Gap(5).AlignItems(ui.Center).Tooltip("Started outside the board: watched read-only").Children(func() {
			ui.Icon(c, iconEye).Size(11, 11).TextColor(p.muted)
			ui.Text(c, filepath.Base(s.Dir)+" · "+tokens(s.Tokens)+" tok").FontSize(11).Font("monospace").TextColor(p.muted).MaxLines(1).Shrink(1).MinWidth(0)
		})
		if s.Failed != "" {
			ui.Text(c, s.Failed).FontSize(11).TextColor(p.red)
		}
	})
}

// ── detail ──────────────────────────────────────────────────────────────────

func detailPane(c *ui.Context, p palette, key string, width float32, body func()) {
	pane(ui.Column(c.Key(key)).Width(width).Shrink(0).Padding(0, 16, 16, 16).Gap(12), p).Children(body)
}

func detailHeader(c *ui.Context, p palette, agent, model string, s Status, label string, closeIt func()) {
	ui.Row(c).Height(titleBarHeight).Shrink(0).Gap(8).AlignItems(ui.Center).DragWindow().Children(func() {
		agentLabel(c, p, agent, s == Running)
		if model != "" {
			ui.Text(c, model).FontSize(11).Font("monospace").TextColor(p.muted)
		}
		ui.Box(c).Grow(1)
		pill(c, label, p.status(s))
		if button(c, p, "close", "", iconX, ghost).Label("Close").Clicked() {
			closeIt()
		}
	})
}

func pill(c *ui.Context, text string, color ui.Color) {
	ui.Text(c, text).FontSize(11).FontWeight(600).TextColor(color).Background(color.Alpha(0.14)).Padding(3, 9).Radius(999)
}

func stat(c *ui.Context, p palette, label, value string) {
	ui.Column(c).Grow(1).Gap(3).Padding(8, 10).Radius(controlRadius).Background(p.column).Children(func() {
		ui.Text(c, label).FontSize(10).Bold().LetterSpacing(0.6).TextColor(p.muted)
		ui.Text(c, value).FontSize(13).Font("monospace").FontFeatures("tnum")
	})
}

func pathLine(c *ui.Context, p palette, dir string) {
	ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
		ui.Icon(c, iconFolder).Size(12, 12).TextColor(p.muted)
		ui.Text(c, dir).FontSize(11.5).Font("monospace").TextColor(p.muted).Selectable().MaxLines(1)
	})
}

// logView shows what the agent did: prompts in the accent, tool calls
// muted, errors red, its words in ink.
func logView(c *ui.Context, p palette, key string, lines []string) {
	ui.Text(c, "ACTIVITY").FontSize(10).Bold().LetterSpacing(0.6).TextColor(p.muted)
	ui.Scroll(c.Key(key)).Grow(1).Gap(6).Padding(12).Radius(10).Background(p.column).Children(func() {
		if len(lines) == 0 {
			ui.Text(c, "No activity yet").FontSize(12).TextColor(p.muted)
		}
		for i, line := range lines {
			color := p.ink
			switch {
			case strings.HasPrefix(line, "› "):
				color = p.brand
			case strings.HasPrefix(line, "▸ "), strings.HasPrefix(line, "── "):
				color = p.muted
			case strings.HasPrefix(line, "✗ "):
				color = p.red
			}
			ui.Text(c.Key(i), line).FontSize(12).Font("monospace").LineHeight(1.45).TextColor(color).Wrap().MaxLines(14).Selectable()
		}
	})
}

// replyView continues the conversation: Enter sends, Shift+Enter breaks the
// line. The agent picks up where it stopped, its session resumed.
func replyView(c *ui.Context, p palette, b *board, task *Task) {
	ui.Row(c).Gap(8).AlignItems(ui.End).Children(func() {
		ui.TextArea(c.Key("reply"), &task.draft).Lines(1, 6).Grow(1).Radius(controlRadius).Background(p.column).
			Placeholder("Reply to " + task.Agent + ": ask for changes or the next step").Label("Reply").
			OnTextKey(func(k ui.TextKey) bool {
				if k.Key != ui.KeyEnter || k.Mods != 0 {
					return false // Shift+Enter breaks the line
				}
				b.reply(task, k.Text())
				k.SetText("")
				return true
			})
		if buttonIf(c, p, "send", "Send", iconArrow, primary, strings.TrimSpace(task.draft) == "").Clicked() {
			b.reply(task, task.draft)
		}
	})
}

func (b *board) detailView(c *ui.Context, p palette, task *Task) {
	detailPane(c, p, fmt.Sprint("task-", task.ID), b.detailWidth(), func() {
		detailHeader(c, p, task.Agent, task.Model, task.Status, string(task.Status), func() { b.selected = nil })
		ui.Text(c, task.Title).FontSize(17).Bold().Wrap()
		meta := fmt.Sprintf("#%d", task.ID)
		if task.From != 0 {
			meta += fmt.Sprintf(" · handed off from #%d", task.From)
		}
		ui.Text(c, meta).FontSize(11.5).TextColor(p.muted)
		pathLine(c, p, task.Dir)
		if ws := task.WS; ws != nil {
			branchLine(c, p, task)
		}
		u := task.Usage
		ui.Row(c).Gap(6).Children(func() {
			stat(c, p, "IN", tokens(u.Input+u.CacheRead+u.CacheWrite))
			stat(c, p, "OUT", tokens(u.Output))
			stat(c, p, "TIME", task.Duration.Round(time.Second).String())
			if u.CostUSD > 0 {
				stat(c, p, "COST", fmt.Sprintf("$%.2f", u.CostUSD))
			} else {
				stat(c, p, "TURNS", fmt.Sprint(task.Runs))
			}
		})
		if task.Err != "" {
			ui.Text(c, task.Err).FontSize(12).TextColor(p.red).Wrap().Padding(8, 10).Radius(controlRadius).Background(p.red.Alpha(0.1))
		}
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			if task.Status == Running {
				if button(c, p, "stop", "Stop", iconStop, secondary).Clicked() {
					b.stop(task)
				}
				return
			}
			if task.Runs == 0 {
				if button(c, p, "run", "Run", iconPlay, primary).Clicked() {
					b.start(task)
				}
			} else if button(c, p, "start-over", "Start over", iconPlay, secondary).Tooltip("Run the task again in a new conversation, on a new branch").Clicked() {
				b.start(task)
			}
			if ws := task.WS; ws != nil && ws.Base != "" && !task.Merged && task.Changes.Files > 0 && (task.Status == Review || task.Status == Done) {
				label := "Merge into " + ws.Base
				if task.merging {
					label = "Merging…"
				}
				if buttonIf(c, p, "merge", label, iconMerge, primary, task.merging).Tooltip("Merge " + ws.Branch + " into " + ws.Base + " in " + ws.Repo).Clicked() {
					b.merge(task)
				}
			}
			ui.Box(c).Grow(1)
			if button(c, p, "delete", "Delete", iconTrash, danger).Clicked() {
				b.remove(task)
			}
		})
		if task.Status == Review || task.Status == Done {
			ui.Row(c).Gap(6).AlignItems(ui.Center).Padding(8, 10).Radius(controlRadius).Background(p.column).Children(func() {
				ui.Text(c, "Hand off to").FontSize(12).TextColor(p.muted).Shrink(0)
				ui.Select(c.Key("handoff-to"), &b.handoffTo, agentNames).Label("Hand off to").Width(130)
				ui.Box(c).Grow(1)
				if button(c, p, "handoff", "Review & continue", iconArrow, secondary).Clicked() {
					b.handoff(task, b.handoffTo)
				}
			})
		}
		if task.WS != nil {
			changes := "Changes"
			if n := task.Changes.Files; n > 0 {
				changes = fmt.Sprintf("Changes · %d", n)
			}
			ui.Segmented(c.Key("detail-tab"), &b.detailTab, "Activity", changes).Label("Detail view")
		}
		if task.WS != nil && b.detailTab == 1 {
			b.changesView(c, p, task)
		} else {
			logView(c, p, "log", append([]string{"› " + firstLine(task.Prompt)}, task.Log...))
		}
		if task.Status != Running && task.Status != Todo && task.SessionRef != "" {
			replyView(c, p, b, task)
		}
	})
}

func (b *board) sessionDetailView(c *ui.Context, p palette, s *Session) {
	detailPane(c, p, "session-"+s.key(), detailWidth, func() {
		st := b.sessionStatus(s)
		label := map[Status]string{Running: "Working", Review: "Needs you", Done: "Done", Failed: "Failed"}[st]
		detailHeader(c, p, s.Agent, s.Model, st, label, func() { b.selectedSession = "" })
		ui.Text(c, s.Title).FontSize(17).Bold().Wrap()
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, iconEye).Size(12, 12).TextColor(p.muted)
			ui.Text(c, "Outside the board · read-only · last active "+ago(s.Updated)).FontSize(11.5).TextColor(p.muted)
		})
		pathLine(c, p, s.Dir)
		u := s.Usage
		ui.Row(c).Gap(6).Children(func() {
			if u != (Usage{}) {
				stat(c, p, "IN", tokens(u.Input))
				stat(c, p, "CACHE", tokens(u.CacheRead+u.CacheWrite))
				stat(c, p, "OUT", tokens(u.Output))
			} else {
				stat(c, p, "TOKENS", tokens(s.Tokens))
			}
		})
		ui.Text(c, "Drag it to Done to dismiss it until it goes on. Idle for 2 hours, it moves there by itself.").FontSize(11.5).TextColor(p.muted).Wrap()
		logView(c, p, "session-log", s.Recent)
	})
}

// ── controls ────────────────────────────────────────────────────────────────

type buttonKind int

const (
	primary buttonKind = iota
	secondary
	ghost
	danger
)

// button is the one button of the board: a fixed radius, an optional icon,
// and a press that gives.
func button(c *ui.Context, p palette, key, label string, icon *ui.SVG, kind buttonKind) ui.Element {
	return buttonIf(c, p, key, label, icon, kind, false)
}

func buttonIf(c *ui.Context, p palette, key, label string, icon *ui.SVG, kind buttonKind, disabled bool) ui.Element {
	e := ui.ButtonBase(c.Key("btn-"+key)).Label(label).Height(30).Padding(0, 12).Radius(controlRadius).Center().Disabled(disabled)
	if label == "" {
		e.Width(30).Padding(0)
	}
	fg, bg := p.ink, ui.Transparent
	hover := e.Hovered() && !disabled
	switch kind {
	case primary:
		fg, bg = ui.Hex("#13160f"), p.lime
		if hover {
			bg = p.lime.Mix(p.ink, 0.06)
		}
	case secondary:
		bg = p.pill
		if hover {
			bg = p.pill.Mix(p.ink, 0.06)
		}
	case ghost, danger:
		if kind == danger {
			fg = p.red
		}
		if hover {
			bg = fg.Alpha(0.08)
		}
	}
	e.Background(bg).TextColor(fg)
	if disabled {
		e.Opacity(0.45)
	}
	scale := float32(1)
	if e.Pressed() && !disabled {
		scale = 0.96
	}
	e.AnimatePose(ui.Pose{Scale: scale}, ui.Snappy)
	e.Children(func() {
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			if icon != nil {
				ui.Icon(c, icon).Size(13, 13)
			}
			if label != "" {
				ui.Text(c, label).FontSize(12.5).FontWeight(600)
			}
		})
	})
	return e
}

// statusDot is a colored dot; a pulsing one breathes a halo, the board's
// sign that something is at work.
func statusDot(c *ui.Context, color ui.Color, pulsing bool) {
	ui.Box(c).Size(8, 8).Children(func() {
		if pulsing {
			phase := c.Now().UnixMilli() / pulse.Milliseconds() % 2
			halo := ui.Box(c).Absolute().Top(-3).Left(-3).Size(14, 14).Radius(7).Background(color.Alpha(0.35))
			pose := ui.Pose{Scale: 0.6, Fade: 0.2}
			if phase == 1 {
				pose = ui.Pose{Scale: 1.15, Fade: 1}
			}
			halo.AnimatePose(pose, ui.Animation{Duration: pulse, Ease: ui.EaseInOut})
			c.After(pulse)
		}
		ui.Box(c).Absolute().Top(0).Left(0).Size(8, 8).Radius(4).Background(color)
	})
}

// ── formatting ──────────────────────────────────────────────────────────────

func tokens(n int) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func cost(usd float64) string {
	if usd == 0 {
		return ""
	}
	return fmt.Sprintf(" · $%.2f", usd)
}

// ago is a short age: now, 9m, 23h, 2d.
func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// detailWidth is the detail pane's: wider while it shows a task's diff.
func (b *board) detailWidth() float32 {
	if t := b.selected; t != nil && t.WS != nil && b.detailTab == 1 {
		return changesWidth
	}
	return detailWidth
}

// ── changes ─────────────────────────────────────────────────────────────────

// branchLine shows where the task's work is: its branch, what it forked
// from, and how much it changed.
func branchLine(c *ui.Context, p palette, t *Task) {
	ws := t.WS
	ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
		ui.Icon(c, iconBranch).Size(12, 12).TextColor(p.muted)
		base := ws.Base
		if base == "" {
			base = ws.BaseSHA[:min(8, len(ws.BaseSHA))]
		}
		ui.Text(c, ws.Branch+" ← "+base).FontSize(11.5).Font("monospace").TextColor(p.muted).Selectable().MaxLines(1).Shrink(1).MinWidth(0)
		if t.Changes.Files > 0 {
			diffStat(c, p, t.Changes.Added, t.Changes.Deleted)
		}
		if t.Merged {
			pill(c, "merged", p.brand)
		}
	}).Tooltip(ws.Worktree)
}

func diffStat(c *ui.Context, p palette, added, deleted int) {
	ui.Row(c).Gap(4).Shrink(0).Children(func() {
		ui.Text(c, fmt.Sprintf("+%d", added)).FontSize(11).Font("monospace").TextColor(p.brand)
		ui.Text(c, fmt.Sprintf("−%d", deleted)).FontSize(11).Font("monospace").TextColor(p.red)
	})
}

// diffPane is the changes tab: the branch's files against its base, and the
// diff of the one chosen. It loads off the main thread, again after a turn.
type diffPane struct {
	task    *Task
	turns   int // the task's turns when it loaded
	loading bool
	files   []fileChange
	err     string
	path    string
	state   *editor.DiffState
	note    string // why the chosen file shows no diff
}

func (b *board) loadChanges(t *Task) {
	ws := *t.WS
	d := &diffPane{task: t, turns: t.Runs, loading: true}
	b.diff = d
	go func() {
		files, err := ws.files()
		b.update(func() {
			if b.diff != d {
				return
			}
			d.loading, d.files = false, files
			if err != nil {
				d.err = err.Error()
			}
			if len(files) > 0 {
				b.openFile(d, files[0].Path)
			}
		})
	}()
}

func (b *board) openFile(d *diffPane, path string) {
	ws := *d.task.WS
	d.path, d.state, d.note = path, nil, ""
	go func() {
		before, after, err := ws.texts(path)
		b.update(func() {
			if b.diff != d || d.path != path {
				return
			}
			switch {
			case err != nil:
				d.note = err.Error()
			case strings.ContainsRune(before, 0) || strings.ContainsRune(after, 0):
				d.note = "Binary file"
			default:
				lang := editor.LanguageFor(path)
				o, n := editor.NewBuffer(before), editor.NewBuffer(after)
				o.SetLanguage(lang)
				n.SetLanguage(lang)
				d.state = &editor.DiffState{Old: o, New: n, Layout: editor.DiffUnified}
			}
		})
	}()
}

func (b *board) changesView(c *ui.Context, p palette, t *Task) {
	if t.Status != Running && (b.diff == nil || b.diff.task != t || b.diff.turns != t.Runs) {
		b.loadChanges(t)
	}
	d := b.diff
	box := ui.Column(c.Key("changes")).Grow(1).MinHeight(0).Gap(8)
	box.Children(func() {
		switch {
		case t.Status == Running && (d == nil || d.task != t):
			ui.Text(c, "The changes show when the turn ends.").FontSize(12).TextColor(p.muted)
			return
		case d == nil || d.task != t || d.loading:
			ui.Spinner(c)
			return
		case d.err != "":
			ui.Text(c, d.err).FontSize(12).TextColor(p.red).Wrap()
			return
		case len(d.files) == 0:
			ui.Text(c, "No changes on "+t.WS.Branch+" yet.").FontSize(12).TextColor(p.muted)
			return
		}
		ui.Scroll(c.Key("files")).MaxHeight(150).Gap(2).Padding(4).Radius(controlRadius).Background(p.column).Children(func() {
			for _, f := range d.files {
				row := ui.ButtonBase(c.Key("file-"+f.Path)).Label(f.Path).FillWidth().Padding(5, 8).Radius(6).Row().Gap(8).AlignItems(ui.Center)
				switch {
				case f.Path == d.path:
					row.Background(p.pane)
				case row.Hovered():
					row.Background(p.ink.Alpha(0.05))
				}
				if row.Clicked() && f.Path != d.path {
					b.openFile(d, f.Path)
				}
				row.Children(func() {
					ui.Text(c, f.Path).FontSize(12).Font("monospace").MaxLines(1).Grow(1).Shrink(1).MinWidth(0)
					if f.Added < 0 {
						ui.Text(c, "binary").FontSize(11).TextColor(p.muted)
					} else {
						diffStat(c, p, f.Added, f.Deleted)
					}
				})
			}
		})
		frame := ui.Column(c.Key("diff")).Grow(1).MinHeight(0).Radius(controlRadius).Clip().Background(p.pane).InsetShadow(0, 0, 0, 1, p.stroke)
		frame.Children(func() {
			switch {
			case d.note != "":
				ui.Text(c, d.note).FontSize(12).TextColor(p.muted).Padding(12)
			case d.state == nil:
				ui.Spinner(c).Margin(12)
			default:
				editor.DiffView(c.Key("diff-"+d.path), d.state).Grow(1).FillWidth()
			}
		})
	})
}
