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
		if edit.Status == "in_progress" && len(edit.Changes) == 0 {
			return "", false
		}
		var b strings.Builder
		for i, c := range edit.Changes {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(opBadge(t, c.Op))
			b.WriteString(" ")
			b.WriteString(t.EditPath.Render(truncate(c.Path, inner-8)))
		}
		if edit.Status == "in_progress" {
			b.WriteString("\n" + t.Faint.Render(rc.frame+" writing"))
			return t.EditBox.Width(rc.width - 2).Render(b.String()), false
		}
		return t.EditBox.Width(rc.width - 2).Render(b.String()), true
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

func renderTool(rc renderCtx, it *item, tool waggle.Tool) (string, bool) {
	t := rc.th
	inner := rc.width - 6
	if inner < 20 {
		inner = 20
	}
	head := t.ToolCmd.Render("$ " + truncate(sanitize(tool.Command), inner))

	if tool.Status == "in_progress" {
		dur := t.ToolDur.Render(" " + rc.now.Sub(it.start).Round(time.Second).String())
		body := head + "\n" + t.Brain.Render(rc.frame) + t.Faint.Render(" running") + dur
		return t.ToolRun.Width(rc.width - 2).Render(body), false
	}

	badge := t.ToolOK.Render("ok")
	if tool.ExitCode != nil && *tool.ExitCode != 0 {
		badge = t.ToolBad.Render(fmt.Sprintf("exit %d", *tool.ExitCode))
	}
	dur := ""
	if !it.start.IsZero() && it.ev.At.After(it.start) {
		dur = t.ToolDur.Render(" " + it.ev.At.Sub(it.start).Round(10*time.Millisecond).String())
	}
	body := head + "  " + badge + dur
	if out := strings.TrimSpace(sanitize(tool.Output)); out != "" {
		lines := 6
		if rc.expanded {
			lines = 1000
		}
		body += "\n" + t.ToolOut.Render(clipLines(out, lines, inner))
	}
	// Expanded state is part of the cache key via the frozen bit: the
	// model unfreezes tool cards when the user toggles ctrl+o.
	return t.ToolBox.Width(rc.width - 2).Render(body), true
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

func clipLines(s string, maxLines, width int) string {
	lines := strings.Split(s, "\n")
	clipped := len(lines) > maxLines
	if clipped {
		lines = lines[:maxLines]
	}
	for i, l := range lines {
		if lipgloss.Width(l) > width {
			lines[i] = truncate(l, width)
		}
	}
	out := strings.Join(lines, "\n")
	if clipped {
		out += "\n…"
	}
	return out
}
