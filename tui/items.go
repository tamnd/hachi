package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"

	"github.com/tamnd/hachi/waggle"
)

// item is one card in the transcript. Rendering is cached per width and
// items freeze once their event can no longer change (the crush pattern:
// finished cards are never re-rendered). Running tool cards stay live so
// the spinner and elapsed time animate.
type item struct {
	ev     waggle.Event
	start  time.Time // first sighting, for tool durations
	width  int
	cached string
	frozen bool
}

// renderCtx carries everything a card render needs beyond its event.
type renderCtx struct {
	th       theme
	md       *mdCache
	width    int
	frame    string // current spinner frame for running cards
	expanded bool   // tool output expanded or clipped
	now      time.Time
}

func (it *item) render(rc renderCtx) string {
	if it.frozen && it.width == rc.width && it.cached != "" {
		return it.cached
	}
	s, done := renderEvent(rc, it)
	it.width, it.cached = rc.width, s
	it.frozen = done
	return s
}

// mdCache memoizes glamour renderers per width, invalidated on resize.
type mdCache struct {
	mu    sync.Mutex
	dark  bool
	rends map[int]*glamour.TermRenderer
}

func newMDCache(dark bool) *mdCache {
	return &mdCache{dark: dark, rends: map[int]*glamour.TermRenderer{}}
}

func (m *mdCache) render(text string, width int) string {
	m.mu.Lock()
	r, ok := m.rends[width]
	if !ok {
		style := "light"
		if m.dark {
			style = "dark"
		}
		var err error
		r, err = glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(width))
		if err != nil {
			m.mu.Unlock()
			return text
		}
		m.rends[width] = r
	}
	m.mu.Unlock()
	out, err := r.Render(text)
	if err != nil {
		return text
	}
	return strings.Trim(out, "\n")
}

func decode[T any](raw json.RawMessage) (T, bool) {
	var v T
	if raw == nil {
		return v, false
	}
	return v, json.Unmarshal(raw, &v) == nil
}

// renderEvent draws one card and reports whether it is final (safe to
// freeze) or still changing (running tools keep animating).
func renderEvent(rc renderCtx, it *item) (string, bool) {
	t, ev := rc.th, it.ev
	inner := rc.width - 4
	if inner < 20 {
		inner = 20
	}
	switch ev.Kind {
	case waggle.KindMessage:
		msg, ok := decode[waggle.Message](ev.Data)
		if !ok {
			return "", true
		}
		if ev.Bee == "human" {
			body := t.Human.Width(rc.width - 2).Render(msg.Text)
			return prefixBar(t.HumanBar.Render("┃ "), body), true
		}
		return rc.md.render(msg.Text, rc.width), true
	case waggle.KindFinding:
		msg, ok := decode[waggle.Message](ev.Data)
		if !ok || strings.TrimSpace(msg.Text) == "" {
			return "", true
		}
		return t.Finding.Width(rc.width - 2).Render("· " + truncate(msg.Text, 400)), true
	case waggle.KindTool:
		tool, ok := decode[waggle.Tool](ev.Data)
		if !ok {
			return "", true
		}
		return renderTool(rc, it, tool)
	case waggle.KindEdit:
		edit, ok := decode[waggle.Edit](ev.Data)
		if !ok {
			return "", true
		}
		return renderEdit(rc, edit)
	case waggle.KindPlan:
		plan, ok := decode[waggle.Plan](ev.Data)
		if !ok || len(plan.Items) == 0 {
			return "", true
		}
		return renderPlan(rc, plan), true
	case waggle.KindDied:
		died, _ := decode[waggle.Died](ev.Data)
		msg := died.Error
		if msg == "" {
			msg = "the run ended unexpectedly"
		}
		return t.DiedBox.Width(rc.width - 2).Render(msg), true
	case waggle.KindRaw:
		raw, _ := decode[waggle.Raw](ev.Data)
		return t.Faint.Render(truncate(raw.Line, inner)), true
	}
	return "", true // spawned, cost, result, need_input live in the status bar
}

// outputClip is how many output lines a collapsed tool card shows.
const outputClip = 6

// renderTool draws a flat tool card: status glyph, the command, timing,
// then clipped output hanging under it. No box; the transcript reads like
// a log, and the hidden-lines line is the expand affordance.
func renderTool(rc renderCtx, it *item, tool waggle.Tool) (string, bool) {
	t := rc.th
	inner := rc.width - 8
	if inner < 20 {
		inner = 20
	}
	cmd := t.ToolCmd.Render("$ " + truncate(sanitize(tool.Command), inner))

	if tool.Status == "in_progress" {
		dur := t.ToolDur.Render("  " + rc.now.Sub(it.start).Round(time.Second).String())
		return t.Brain.Render(rc.frame) + " " + cmd + dur, false
	}

	icon := t.ToolOK.Render("✓")
	tag := ""
	if tool.ExitCode != nil && *tool.ExitCode != 0 {
		icon = t.ToolBad.Render("×")
		tag = t.ToolBad.Render(fmt.Sprintf("  exit %d", *tool.ExitCode))
	}
	dur := ""
	if !it.start.IsZero() && it.ev.At.After(it.start) {
		dur = t.ToolDur.Render("  " + it.ev.At.Sub(it.start).Round(10*time.Millisecond).String())
	}
	head := icon + " " + cmd + tag + dur
	out := strings.TrimSpace(sanitize(tool.Output))
	if out == "" {
		return head, true
	}
	// Expanded state is part of the cache key via the frozen bit: the
	// model unfreezes tool cards when the user toggles ctrl+o.
	return head + "\n" + hangOutput(t, out, rc.expanded, inner), true
}

// hangOutput indents output under its card and clips it, ending with the
// hidden-line count that doubles as the expand hint.
func hangOutput(t theme, s string, expanded bool, width int) string {
	lines := strings.Split(s, "\n")
	hidden := 0
	if !expanded && len(lines) > outputClip {
		hidden = len(lines) - outputClip
		lines = lines[:outputClip]
	}
	for i, l := range lines {
		if lipgloss.Width(l) > width {
			l = truncate(l, width)
		}
		prefix := "    "
		if i == 0 {
			prefix = "  └ "
		}
		lines[i] = t.Faint.Render(prefix) + t.ToolOut.Render(l)
	}
	if hidden > 0 {
		lines = append(lines, t.Faint.Render(fmt.Sprintf("    … +%d lines (ctrl+o to expand)", hidden)))
	}
	return strings.Join(lines, "\n")
}

// renderEdit draws file changes flat: one line for a single file, a
// hanging list when the brain touched several.
func renderEdit(rc renderCtx, edit waggle.Edit) (string, bool) {
	t := rc.th
	inner := rc.width - 10
	if inner < 20 {
		inner = 20
	}
	running := edit.Status == "in_progress"
	if running && len(edit.Changes) == 0 {
		return "", false
	}
	icon := t.ToolOK.Render("✓")
	if running {
		icon = t.Brain.Render(rc.frame)
	}
	head := icon + " " + t.ToolCmd.Render("edit")
	if len(edit.Changes) == 1 {
		c := edit.Changes[0]
		return head + "  " + opBadge(t, c.Op) + " " + t.EditPath.Render(truncate(c.Path, inner)), !running
	}
	head += t.ToolDur.Render(fmt.Sprintf("  %d files", len(edit.Changes)))
	var b strings.Builder
	b.WriteString(head)
	for i, c := range edit.Changes {
		prefix := "    "
		if i == 0 {
			prefix = "  └ "
		}
		b.WriteString("\n" + t.Faint.Render(prefix) + opBadge(t, c.Op) + " " + t.EditPath.Render(truncate(c.Path, inner)))
	}
	return b.String(), !running
}

// renderPlan draws the brain's checklist flat: done steps get a check, the
// first open step is the current one and gets the arrow.
func renderPlan(rc renderCtx, plan waggle.Plan) string {
	t := rc.th
	inner := rc.width - 6
	if inner < 20 {
		inner = 20
	}
	done := 0
	for _, it := range plan.Items {
		if it.Done {
			done++
		}
	}
	var b strings.Builder
	b.WriteString(t.Brain.Render("●") + " " + t.Title.Render("plan") + t.ToolDur.Render(fmt.Sprintf("  %d/%d", done, len(plan.Items))))
	current := true
	for _, it := range plan.Items {
		b.WriteString("\n  ")
		switch {
		case it.Done:
			b.WriteString(t.ToolOK.Render("✓ ") + t.Faint.Render(truncate(it.Text, inner)))
		case current:
			b.WriteString(t.Brain.Render("→ ") + truncate(it.Text, inner))
			current = false
		default:
			b.WriteString(t.Faint.Render("○ " + truncate(it.Text, inner)))
		}
	}
	return b.String()
}

func opBadge(t theme, op string) string {
	switch op {
	case "add":
		return t.EditAdd.Render("+")
	case "delete":
		return t.EditDel.Render("-")
	default:
		return t.EditPath.Render("~")
	}
}

func prefixBar(bar, body string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		lines[i] = bar + l
	}
	return strings.Join(lines, "\n")
}

// sanitize strips ANSI escape sequences and control characters (except
// newline and tab) from upstream text so tool output cannot corrupt the
// screen. Styling is hachi's job, not the tool's.
func sanitize(s string) string {
	if !strings.ContainsAny(s, "\x1b\x00\x07\x08\r") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	esc := false
	osc := false
	for _, r := range s {
		switch {
		case osc:
			if r == '\a' || r == '\\' {
				osc = false
			}
		case esc:
			if r == ']' {
				esc, osc = false, true
			} else if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				esc = false
			}
		case r == '\x1b':
			esc = true
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			// drop other control characters, including \r
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if n < 1 {
		n = 1
	}
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
