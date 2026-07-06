package tui

// The attention strip: one quiet row above every screen that answers
// "does anything else want me" without leaving the current session.
// With one session and nothing pending it stays hidden, so hachi with
// a single session reads like a plain chat. It never animates, and
// only the need-you segment gets the full-strength attention color;
// everything else stays faint. The counts come from the same session
// list a board would use: the strip and the board are two renderings
// of one derivation.

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/tamnd/hachi/hive"
)

type stripTickMsg struct{}

// stripTick re-reads the session list every couple of seconds. Slow on
// purpose: the strip is peripheral vision, not a live feed.
func stripTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return stripTickMsg{} })
}

// pollSessions refreshes the list without surfacing errors. A failed
// background poll must not stamp an error into the focused composer,
// so unlike loadSessions it swallows them and tries again next tick.
func (m *model) pollSessions() tea.Cmd {
	return func() tea.Msg {
		list, err := m.svc.Sessions(context.Background())
		if err != nil {
			return nil
		}
		return sessionsMsg{list}
	}
}

// stripCounts is the strip's numbers, summed from the session list.
type stripCounts struct {
	needs   int // blocked on the human, or died
	working int
	ready   int // idle with changes nobody accepted yet
	waiting int // queued behind a busy folder
}

func (m *model) stripCounts() stripCounts {
	var c stripCounts
	for _, s := range m.sessions {
		switch {
		case s.State == hive.StateNeeds || s.State == hive.StateDied:
			c.needs++
		case s.State == hive.StateWorking:
			c.working++
		case s.State == hive.StateWaiting:
			c.waiting++
		case s.DiffReady:
			c.ready++
		}
	}
	return c
}

// stripVisible says whether the strip earns its row: something needs
// the human, more than one session is open, or a session out of view
// is doing anything at all.
func (m *model) stripVisible() bool {
	c := m.stripCounts()
	if c.needs > 0 || len(m.open) > 1 {
		return true
	}
	for _, s := range m.sessions {
		if s.ID == m.sess.ID {
			continue
		}
		if s.State == hive.StateWorking || s.State == hive.StateWaiting ||
			(s.State == hive.StateIdle && s.DiffReady) {
			return true
		}
	}
	return false
}

// stripRows is what layout subtracts: the strip's height when showing.
func (m *model) stripRows() int {
	if m.stripOn {
		return 1
	}
	return 0
}

// viewStrip renders the row. Segments sit in priority order and drop
// from the right when the terminal is narrow, so need-you goes last;
// the key hints only fit on wide terminals and drop first.
func (m *model) viewStrip() string {
	c := m.stripCounts()
	sep := m.th.Faint.Render(" · ")
	var segs []string
	if c.needs > 0 {
		word := "need you"
		if c.needs == 1 {
			word = "needs you"
		}
		segs = append(segs, m.th.ToolBad.Render(fmt.Sprintf("● %d %s", c.needs, word)))
	}
	if c.working > 0 {
		segs = append(segs, m.th.Faint.Render(fmt.Sprintf("▸ %d working", c.working)))
	}
	if c.ready > 0 {
		word := "diff ready"
		if c.ready > 1 {
			word = "diffs ready"
		}
		segs = append(segs, m.th.Faint.Render(fmt.Sprintf("◐ %d %s", c.ready, word)))
	}
	if c.waiting > 0 {
		segs = append(segs, m.th.Faint.Render(fmt.Sprintf("◌ %d waiting", c.waiting)))
	}
	if len(segs) == 0 {
		// Visible only because several sessions are open, all of them
		// quiet. Say that rather than draw a blank row.
		segs = append(segs, m.th.Faint.Render(fmt.Sprintf("○ %d open", len(m.open))))
	}
	hints := m.th.Faint.Render("│ ") +
		m.th.StatusKey.Render("ctrl+l") + m.th.Faint.Render(" sessions") + sep +
		m.th.StatusKey.Render("ctrl+n") + m.th.Faint.Render(" new")
	build := func(n int, withHints bool) string {
		line := " " + strings.Join(segs[:n], sep)
		if withHints {
			line += "  " + hints
		}
		return line
	}
	line := build(len(segs), m.w >= 120)
	if lipgloss.Width(line) > m.w {
		line = build(len(segs), false)
	}
	for n := len(segs); lipgloss.Width(line) > m.w && n > 1; {
		n--
		line = build(n, false)
	}
	return line
}
