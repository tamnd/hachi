package tui

// The diff-so-far screen: one key from anywhere in a session, one
// scrolling pane, per-file sections. Everything shown was computed by
// the engine against the baseline; this file only colors it. Nothing
// auto-refreshes and nothing is cached, so what R shows is the tree at
// the moment R was pressed.

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/tamnd/hachi/hive"
)

type diffMsg struct {
	files []hive.FileDiff
	err   error
}

func (m *model) loadDiff() tea.Cmd {
	id := m.sess.ID
	return func() tea.Msg {
		files, err := m.svc.Changes(context.Background(), id)
		return diffMsg{files: files, err: err}
	}
}

func (m *model) openDiff() (tea.Model, tea.Cmd) {
	m.screen = screenDiff
	m.diff, m.diffErr, m.diffMarks = nil, "", nil
	m.diffLoading = true
	m.layout()
	// Opening the diff is seeing it: a finished run waiting on review
	// stops needing you and parks in review.
	return m, tea.Batch(m.loadDiff(), m.seen(m.sess.ID))
}

func (m *model) applyDiff(msg diffMsg) {
	m.diffLoading = false
	if msg.err != nil {
		m.diffErr = msg.err.Error()
		return
	}
	m.diffErr = ""
	m.diff = msg.files
	m.renderDiff()
}

func (m *model) keyDiff(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.quitAll()
		return m, tea.Quit
	case "q", "esc", "d":
		m.screen = screenChat
		return m, nil
	case "r":
		return m.openReview()
	case "R":
		return m, m.loadDiff()
	case "n":
		for _, mark := range m.diffMarks {
			if mark > m.dvp.YOffset() {
				m.dvp.SetYOffset(mark)
				break
			}
		}
		return m, nil
	case "p":
		for i := len(m.diffMarks) - 1; i >= 0; i-- {
			if m.diffMarks[i] < m.dvp.YOffset() {
				m.dvp.SetYOffset(m.diffMarks[i])
				break
			}
		}
		return m, nil
	case "g":
		m.dvp.GotoTop()
		return m, nil
	case "G":
		m.dvp.GotoBottom()
		return m, nil
	}
	var cmd tea.Cmd
	m.dvp, cmd = m.dvp.Update(msg) // j/k, arrows, pgup/pgdown
	return m, cmd
}

// renderDiff builds the pane's content and remembers where each file's
// section starts, so n and p can jump between files.
func (m *model) renderDiff() {
	var b strings.Builder
	m.diffMarks = m.diffMarks[:0]
	line := 0
	put := func(s string) {
		b.WriteString(s)
		b.WriteString("\n")
		line += strings.Count(s, "\n") + 1
	}
	for i, f := range m.diff {
		if i > 0 {
			put("")
		}
		m.diffMarks = append(m.diffMarks, line)
		put(m.diffFileHeader(f))
		if f.Note != "" {
			put("  " + m.th.Finding.Render(f.Note))
		}
		if f.Patch != "" {
			for pl := range strings.SplitSeq(f.Patch, "\n") {
				put(m.diffLine(pl))
			}
		}
	}
	m.dvp.SetContent(strings.TrimRight(b.String(), "\n"))
	m.dvp.GotoTop()
}

func (m *model) diffFileHeader(f hive.FileDiff) string {
	var letter string
	switch f.Status {
	case "A":
		letter = m.th.EditAdd.Render("A")
	case "D":
		letter = m.th.EditDel.Render("D")
	default:
		letter = m.th.Brain.Render("M")
	}
	s := letter + " " + m.th.EditPath.Bold(true).Render(f.Path)
	if adds, dels := patchTally(f.Patch); adds+dels > 0 {
		s += "  " + m.th.EditAdd.Render(fmt.Sprintf("+%d", adds)) +
			" " + m.th.EditDel.Render(fmt.Sprintf("-%d", dels))
	}
	if f.Outside {
		s += "  " + m.th.ToolBad.Render("· you changed this after")
	}
	return s
}

func (m *model) diffLine(pl string) string {
	switch {
	case strings.HasPrefix(pl, "@@"):
		return m.th.Faint.Render(pl)
	case strings.HasPrefix(pl, "+"):
		return m.th.EditAdd.Render(pl)
	case strings.HasPrefix(pl, "-"):
		return m.th.EditDel.Render(pl)
	}
	return m.th.ToolOut.Render(pl)
}

// patchTally counts added and removed lines in a hunks-only patch.
func patchTally(patch string) (adds, dels int) {
	if patch == "" {
		return 0, 0
	}
	for pl := range strings.SplitSeq(patch, "\n") {
		switch {
		case strings.HasPrefix(pl, "@@"):
		case strings.HasPrefix(pl, "+"):
			adds++
		case strings.HasPrefix(pl, "-"):
			dels++
		}
	}
	return adds, dels
}

func (m *model) viewDiff() string {
	title := m.th.Title.Render("changes so far")
	var meta []string
	adds, dels := 0, 0
	for _, f := range m.diff {
		a, d := patchTally(f.Patch)
		adds += a
		dels += d
	}
	switch {
	case m.diffLoading:
		meta = append(meta, "computing…")
	case len(m.diff) == 1:
		meta = append(meta, "1 file")
	default:
		meta = append(meta, fmt.Sprintf("%d files", len(m.diff)))
	}
	if adds+dels > 0 {
		meta = append(meta, m.th.EditAdd.Render(fmt.Sprintf("+%d", adds))+" "+m.th.EditDel.Render(fmt.Sprintf("-%d", dels)))
	}
	if m.working {
		meta = append(meta, "run still going")
	}
	header := " " + title + m.th.Faint.Render("  ──  "+strings.Join(meta, " · "))

	body := m.dvp.View()
	switch {
	case m.diffErr != "":
		body = "\n " + m.th.ToolBad.Render("× "+m.diffErr)
	case m.diffLoading:
		body = "\n " + m.th.Faint.Render("reading the tree…")
	case len(m.diff) == 0:
		body = "\n " + m.th.Faint.Render("no changes yet — the tree still matches the baseline")
	}

	hints := " " + m.th.StatusKey.Render("j/k") + m.th.Faint.Render(" scroll · ") +
		m.th.StatusKey.Render("n/p") + m.th.Faint.Render(" file · ") +
		m.th.StatusKey.Render("r") + m.th.Faint.Render(" review · ") +
		m.th.StatusKey.Render("R") + m.th.Faint.Render(" refresh · ") +
		m.th.StatusKey.Render("q") + m.th.Faint.Render(" back")
	return header + "\n" + body + "\n" + hints
}
