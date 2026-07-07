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
	"github.com/tamnd/hachi/waggle"
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
		if s.ID == m.sess.ID {
			// The session on screen speaks for itself; the strip only
			// counts what is happening elsewhere.
			continue
		}
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

// topNeed picks the raised reason worth naming on a wide strip: the
// highest-priority reason among sessions elsewhere, oldest raise first
// within a rank. Questions outrank deaths outrank waiting diffs.
func (m *model) topNeed() (title, detail string) {
	best := -1
	for i, s := range m.sessions {
		if s.ID == m.sess.ID || (s.State != hive.StateNeeds && s.State != hive.StateDied) {
			continue
		}
		if best == -1 || needRank(s.Reason) < needRank(m.sessions[best].Reason) ||
			(needRank(s.Reason) == needRank(m.sessions[best].Reason) && s.Raised.Before(m.sessions[best].Raised)) {
			best = i
		}
	}
	if best == -1 {
		return "", ""
	}
	s := m.sessions[best]
	title = s.Title
	if title == "" {
		title = "a session"
	}
	detail = s.Detail
	if detail == "" {
		switch s.Reason {
		case "died":
			detail = "the run died"
		case "stall":
			detail = "quiet too long"
		case "diff":
			detail = "changes to review"
		default:
			detail = "waiting on you"
		}
	}
	return title, detail
}

// keepWaiting forwards the w press: the engine clears the stall and
// doubles this turn's quiet threshold. The local raise clears at once
// so the notice drops without waiting a poll.
func (m *model) keepWaiting() tea.Cmd {
	id := m.sess.ID
	m.sess.Reason, m.sess.Detail = "", ""
	return func() tea.Msg {
		_ = m.svc.KeepWaiting(context.Background(), id)
		return nil
	}
}

// seen tells the engine the human has looked: opening a diff parks its
// raise back in review, focusing a died session acknowledges the death.
// Fire and forget; the next poll reflects it.
func (m *model) seen(id waggle.SessionID) tea.Cmd {
	if id == "" {
		return nil
	}
	return func() tea.Msg {
		_ = m.svc.Seen(context.Background(), id)
		return nil
	}
}

// autoSeen acknowledges what the human is already looking at: a died
// session they are inside, or a fresh diff whose screen is open. It runs
// off the 2s poll, so a turn that ends while its diff is on screen
// clears itself within a beat instead of nagging.
func (m *model) autoSeen() tea.Cmd {
	for _, s := range m.sessions {
		if s.ID != m.sess.ID || s.ID == "" {
			continue
		}
		if s.State == hive.StateDied ||
			(s.Reason == "diff" && (m.screen == screenDiff || m.screen == screenReview)) {
			return m.seen(s.ID)
		}
	}
	return nil
}

// viewStrip renders the row. Segments sit in priority order and drop
// from the right when the terminal is narrow, so need-you goes last; the
// key hints only fit on wide terminals and drop first, then the named
// reason, then the counts.
func (m *model) viewStrip() string {
	c := m.stripCounts()
	sep := m.th.Faint.Render(" · ")
	var segs []string
	var reason string
	if c.needs > 0 {
		word := "need you"
		if c.needs == 1 {
			word = "needs you"
		}
		segs = append(segs, m.th.ToolBad.Render(fmt.Sprintf("● %d %s", c.needs, word)))
		if title, detail := m.topNeed(); title != "" && m.w >= 120 {
			reason = m.th.Faint.Render(" (" + truncate(title, 24) + ": " + truncate(detail, m.w/3) + ")")
		}
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
	build := func(n int, withHints, withReason bool) string {
		parts := append([]string{}, segs[:n]...)
		if withReason && reason != "" {
			parts[0] += reason
		}
		line := " " + strings.Join(parts, sep)
		if withHints {
			line += "  " + hints
		}
		return line
	}
	line := build(len(segs), m.w >= 120, true)
	if lipgloss.Width(line) > m.w {
		line = build(len(segs), false, true)
	}
	if lipgloss.Width(line) > m.w {
		line = build(len(segs), false, false)
	}
	for n := len(segs); lipgloss.Width(line) > m.w && n > 1; {
		n--
		line = build(n, false, false)
	}
	return line
}
