package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/tamnd/hachi/waggle"
)

// item is one renderable block in the transcript. Rendering is cached per
// width, and items freeze once their event can no longer change (the
// crush pattern: frozen items are never re-rendered).
type item struct {
	ev     waggle.Event
	width  int
	cached string
	frozen bool
}

func (it *item) render(t theme, md *mdCache, width int) string {
	if it.frozen && it.width == width && it.cached != "" {
		return it.cached
	}
	s := renderEvent(t, md, it.ev, width)
	it.width, it.cached = width, s
	it.frozen = true // S0 events arrive complete; live cards land in S1
	return s
}

// mdCache memoizes glamour renderers per width, invalidated on resize.
type mdCache struct {
	mu sync.Mutex
	r  map[int]*glamour.TermRenderer
}

func newMDCache() *mdCache { return &mdCache{r: map[int]*glamour.TermRenderer{}} }

func (m *mdCache) render(text string, width int) string {
	m.mu.Lock()
	r, ok := m.r[width]
	if !ok {
		var err error
		r, err = glamour.NewTermRenderer(glamour.WithAutoStyle(), glamour.WithWordWrap(width))
		if err != nil {
			m.mu.Unlock()
			return text
		}
		m.r[width] = r
	}
	m.mu.Unlock()
	out, err := r.Render(text)
	if err != nil {
		return text
	}
	return strings.TrimRight(out, "\n")
}

func decode[T any](raw json.RawMessage) (T, bool) {
	var v T
	if raw == nil {
		return v, false
	}
	return v, json.Unmarshal(raw, &v) == nil
}

func renderEvent(t theme, md *mdCache, ev waggle.Event, width int) string {
	inner := width - 4
	if inner < 20 {
		inner = 20
	}
	switch ev.Kind {
	case waggle.KindMessage:
		msg, ok := decode[waggle.Message](ev.Data)
		if !ok {
			return ""
		}
		if ev.Bee == "human" {
			return t.HumanTag.Render("you ") + t.Human.Render(msg.Text)
		}
		return md.render(msg.Text, width)
	case waggle.KindFinding:
		msg, ok := decode[waggle.Message](ev.Data)
		if !ok || strings.TrimSpace(msg.Text) == "" {
			return ""
		}
		return t.Finding.Render(truncate(msg.Text, 400))
	case waggle.KindTool:
		tool, ok := decode[waggle.Tool](ev.Data)
		if !ok {
			return ""
		}
		if tool.Status == "in_progress" {
			return "" // completed card carries everything at S0
		}
		head := t.ToolCmd.Render("$ " + truncate(tool.Command, inner))
		badge := t.ToolOK.Render("ok")
		if tool.ExitCode != nil && *tool.ExitCode != 0 {
			badge = t.ToolBad.Render(fmt.Sprintf("exit %d", *tool.ExitCode))
		}
		body := head + "  " + badge
		if out := strings.TrimSpace(tool.Output); out != "" {
			body += "\n" + t.ToolOut.Render(clipLines(out, 6, inner))
		}
		return t.ToolBox.Width(width - 2).Render(body)
	case waggle.KindEdit:
		edit, ok := decode[waggle.Edit](ev.Data)
		if !ok || edit.Status == "in_progress" {
			return ""
		}
		var b strings.Builder
		for i, c := range edit.Changes {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(t.EditPath.Render("✎ " + c.Path))
			b.WriteString(t.Faint.Render(" (" + c.Op + ")"))
		}
		return t.EditBox.Width(width - 2).Render(b.String())
	case waggle.KindDied:
		died, _ := decode[waggle.Died](ev.Data)
		msg := died.Error
		if msg == "" {
			msg = "the run ended unexpectedly"
		}
		return t.DiedBox.Width(width - 2).Render(msg)
	case waggle.KindRaw:
		raw, _ := decode[waggle.Raw](ev.Data)
		return t.Faint.Render(truncate(raw.Line, inner))
	}
	return "" // spawned, cost, result, need_input render in the status bar
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
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
