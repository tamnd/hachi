package tui

// The review screen: the boundary where agent work becomes your work.
// File tree left, diff right, and four verbs that all run engine-side
// through Service: a stage, A stage+commit through an editable draft,
// d restore, u undo everything. The TUI sends intent and re-renders
// from the result; it never touches git itself. One rule is absolute:
// no path from agent output to a commit skips the draft editor.

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/tamnd/hachi/hive"
)

type stagedMsg struct {
	paths []string
	err   error
}
type draftMsg struct {
	text string
	err  error
}
type committedMsg struct {
	out string
	err error
}
type restoredMsg struct {
	rep     hive.RestoreReport
	blanket bool
	err     error
}

func (m *model) openReview() (tea.Model, tea.Cmd) {
	m.screen = screenReview
	m.rvSel, m.rvFocus, m.rvDraft = 0, false, false
	m.rvConfirm, m.rvStatus = "", ""
	m.rvAskKeep, m.rvKeep, m.rvBtn = false, nil, 0
	m.rvGone = map[string]bool{}
	if !m.rvChose {
		// Outside a git repo the sentence view is the default; a session
		// in a repo lands on the file tree. The user's own toggle wins.
		m.rvPlain = !m.sess.InRepo
	}
	m.layout()
	m.renderReview()
	return m, m.loadDiff() // a fresh set; the diff screen's copy may be stale
}

// reviewCmds — each verb is one Service call in a tea.Cmd.

func (m *model) stage(paths []string) tea.Cmd {
	id := m.sess.ID
	return func() tea.Msg {
		staged, err := m.svc.Stage(context.Background(), id, paths)
		return stagedMsg{paths: staged, err: err}
	}
}

// stageAndDraft is A in one command: everything staged, then the draft.
// One closure keeps the order without a message round-trip between.
func (m *model) stageAndDraft() tea.Cmd {
	id := m.sess.ID
	return func() tea.Msg {
		if _, err := m.svc.Stage(context.Background(), id, nil); err != nil {
			return stagedMsg{err: err}
		}
		text, err := m.svc.CommitDraft(context.Background(), id)
		return draftMsg{text: text, err: err}
	}
}

func (m *model) commit(message string) tea.Cmd {
	id := m.sess.ID
	return func() tea.Msg {
		out, err := m.svc.Commit(context.Background(), id, message)
		return committedMsg{out: out, err: err}
	}
}

func (m *model) restore(paths []string) tea.Cmd {
	id := m.sess.ID
	return func() tea.Msg {
		rep, err := m.svc.Restore(context.Background(), id, paths)
		return restoredMsg{rep: rep, blanket: paths == nil, err: err}
	}
}

// applyReview folds a verb's result back into the screen.

func (m *model) applyStaged(msg stagedMsg) tea.Cmd {
	if msg.err != nil {
		m.rvStatus = "× " + msg.err.Error()
		return nil
	}
	// The same verb wears different words per audience: "staged" is git
	// truth in the tree view, "kept" is what actually happened everywhere
	// the word stage would be jargon.
	verb := "staged"
	if m.rvPlain || !m.sess.InRepo {
		verb = "kept"
	}
	m.rvStatus = countLine(len(msg.paths), verb)
	if m.rvNote != "" {
		m.rvStatus += " · " + m.rvNote
		m.rvNote = ""
	}
	return m.loadDiff()
}

func (m *model) applyDraft(msg draftMsg) tea.Cmd {
	if msg.err != nil {
		if strings.Contains(msg.err.Error(), "not inside a git repository") {
			m.rvStatus = "this folder is not a git repo, changes are kept in place"
		} else {
			m.rvStatus = "× " + msg.err.Error()
		}
		return nil
	}
	m.rvTA.SetValue(strings.TrimRight(msg.text, "\n"))
	m.rvTA.MoveToEnd()
	m.rvDraft = true
	// The reload draws the staged markers A just created behind the box.
	return tea.Batch(m.rvTA.Focus(), m.loadDiff())
}

func (m *model) applyCommitted(msg committedMsg) tea.Cmd {
	m.rvDraft = false
	if msg.err != nil {
		// Hook output verbatim, in a plain failure block. Staging is
		// untouched, so the user can fix and A again.
		m.rvStatus = "× commit failed"
		if msg.out != "" {
			m.rvStatus = "× " + firstLine(msg.out)
		}
		return nil
	}
	m.rvStatus = firstLine(msg.out)
	return m.loadDiff()
}

func (m *model) applyRestored(msg restoredMsg) tea.Cmd {
	if msg.err != nil {
		m.rvStatus = "× " + msg.err.Error()
		return nil
	}
	for _, p := range msg.rep.Restored {
		m.rvGone[p] = true
	}
	m.rvStatus = restoreLine(msg.rep)
	if msg.blanket && len(msg.rep.Skipped) == 0 {
		// Nothing left to review; the transcript marker already says
		// what happened.
		m.screen = screenChat
		return nil
	}
	return m.loadDiff()
}

func restoreLine(rep hive.RestoreReport) string {
	s := countLine(len(rep.Restored), "restored")
	if n := len(rep.Skipped); n == 1 {
		s += fmt.Sprintf(", skipped 1 (%s)", rep.Skipped[0].Reason)
	} else if n > 1 {
		s += fmt.Sprintf(", skipped %d", n)
	}
	return s
}

func countLine(n int, verb string) string {
	if n == 1 {
		return "1 file " + verb
	}
	return fmt.Sprintf("%d files %s", n, verb)
}

func firstLine(s string) string {
	return strings.SplitN(strings.TrimSpace(s), "\n", 2)[0]
}

// --- keys ---

func (m *model) keyReview(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.rvDraft {
		return m.keyDraft(msg)
	}
	if m.rvConfirm != "" {
		return m.keyConfirm(msg)
	}
	if m.rvAskKeep {
		return m.keyKeepConfirm(msg)
	}
	if m.rvPlain {
		return m.keySentence(msg)
	}
	switch msg.String() {
	case "ctrl+c":
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	case "q", "esc":
		if m.rvFocus {
			m.rvFocus = false
			return m, nil
		}
		m.screen = screenChat
		return m, nil
	case "enter":
		m.rvFocus = true
		return m, nil
	case "j", "down":
		if m.rvFocus {
			m.rvVP.ScrollDown(1)
		} else if m.rvSel < len(m.diff) {
			m.rvSel++
			m.renderReview()
		}
		return m, nil
	case "k", "up":
		if m.rvFocus {
			m.rvVP.ScrollUp(1)
		} else if m.rvSel > 0 {
			m.rvSel--
			m.renderReview()
		}
		return m, nil
	case "g":
		m.rvVP.GotoTop()
		return m, nil
	case "G":
		m.rvVP.GotoBottom()
		return m, nil
	case "a":
		if f, ok := m.rvFile(); ok {
			return m, m.keep([]string{f.Path})
		}
		return m, m.keep(nil)
	case "s":
		m.rvPlain, m.rvChose = true, true
		return m, nil
	case "A":
		if !m.sess.InRepo {
			// A degrades to a plus a note: there is nothing to commit
			// to, the kept files just stay in place.
			m.rvNote = "this folder is not a git repo, changes are kept in place"
			return m, m.keep(nil)
		}
		// Stage everything, then the draft; commit only ever happens
		// from inside the draft editor.
		return m, m.stageAndDraft()
	case "d":
		if f, ok := m.rvFile(); ok {
			m.rvConfirm = f.Path
			return m, nil
		}
		m.rvConfirm = "*"
		return m, nil
	case "u":
		m.rvConfirm = "*"
		return m, nil
	case "r":
		return m.requestChanges()
	case "R":
		return m, m.loadDiff()
	}
	if m.rvFocus {
		var cmd tea.Cmd
		m.rvVP, cmd = m.rvVP.Update(msg)
		return m, cmd
	}
	return m, nil
}

// keyConfirm is the one-line restore confirm: y runs it, anything that
// reads as no cancels.
func (m *model) keyConfirm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	target := m.rvConfirm
	m.rvConfirm = ""
	switch msg.String() {
	case "y", "Y", "enter":
		if target == "*" {
			return m, m.restore(nil)
		}
		return m, m.restore([]string{target})
	}
	return m, nil
}

// keep is a with one wrinkle: outside a git repo, keeping is the one
// action that narrows Undo, so the first one asks in a sentence before
// it runs. In a repo, staging narrows nothing and runs straight away.
func (m *model) keep(paths []string) tea.Cmd {
	if !m.sess.InRepo && !m.keptOnce {
		m.rvAskKeep, m.rvKeep = true, paths
		return nil
	}
	return m.stage(paths)
}

func (m *model) keyKeepConfirm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	paths := m.rvKeep
	m.rvAskKeep, m.rvKeep = false, nil
	switch msg.String() {
	case "y", "Y", "enter":
		m.keptOnce = true
		return m, m.stage(paths)
	}
	return m, nil
}

func (m *model) keyDraft(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	case "esc":
		// Abandoning keeps the staging; that is just the a state.
		m.rvDraft = false
		m.rvStatus = "draft abandoned, staging kept"
		return m, nil
	case "ctrl+d":
		text := strings.TrimSpace(m.rvTA.Value())
		if text == "" {
			m.rvStatus = "× the commit message is empty"
			m.rvDraft = false
			return m, nil
		}
		return m, m.commit(text + "\n")
	}
	var cmd tea.Cmd
	m.rvTA, cmd = m.rvTA.Update(msg)
	return m, cmd
}

// requestChanges returns to the composer with the diff riding along as
// context for the next message. Nothing is restored, staged, or
// committed; it is a conversational turn with better evidence.
func (m *model) requestChanges() (tea.Model, tea.Cmd) {
	m.reqDiff = diffAsText(m.diff)
	if n := len(m.diff); n == 1 {
		m.reqBanner = "requesting changes on 1 file"
	} else {
		m.reqBanner = fmt.Sprintf("requesting changes on %d files", n)
	}
	m.screen = screenChat
	return m, m.ta.Focus()
}

// diffAsText flattens the change set for attachment to a message.
func diffAsText(diffs []hive.FileDiff) string {
	var b strings.Builder
	for _, f := range diffs {
		fmt.Fprintf(&b, "%s %s\n", f.Status, f.Path)
		if f.Note != "" {
			b.WriteString(f.Note + "\n")
		}
		if f.Patch != "" {
			b.WriteString(f.Patch + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// rvFile returns the selected file row; false on the summary row.
func (m *model) rvFile() (hive.FileDiff, bool) {
	if m.rvSel == 0 || m.rvSel > len(m.diff) {
		return hive.FileDiff{}, false
	}
	return m.diff[m.rvSel-1], true
}

// --- view ---

// rvTreeWidth is the left pane's width for the current window.
func (m *model) rvTreeWidth() int {
	w := m.w / 3
	if w > 34 {
		w = 34
	}
	if w < 18 {
		w = 18
	}
	return w
}

// renderReview refills the diff pane for the current selection: the
// whole set on the summary row, one file otherwise.
func (m *model) renderReview() {
	var b strings.Builder
	put := func(s string) { b.WriteString(s); b.WriteString("\n") }
	render := func(f hive.FileDiff) {
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
	if f, ok := m.rvFile(); ok {
		render(f)
	} else {
		for i, f := range m.diff {
			if i > 0 {
				put("")
			}
			render(f)
		}
	}
	m.rvVP.SetContent(strings.TrimRight(b.String(), "\n"))
	m.rvVP.GotoTop()
}

// rvMarker is the tree's per-file state glyph: staged, pending, or
// restored this visit.
func (m *model) rvMarker(f hive.FileDiff) string {
	switch {
	case m.rvGone[f.Path]:
		return m.th.Faint.Render("✗")
	case f.Staged:
		return m.th.EditAdd.Render("●")
	}
	return m.th.Faint.Render("○")
}

func (m *model) viewReview() string {
	if m.rvPlain {
		return m.viewSentence()
	}
	staged := 0
	for _, f := range m.diff {
		if f.Staged {
			staged++
		}
	}
	adds, dels := 0, 0
	for _, f := range m.diff {
		a, d := patchTally(f.Patch)
		adds, dels = adds+a, dels+d
	}
	meta := countLine(len(m.diff), "changed")
	if staged == len(m.diff) && staged > 0 {
		meta = countLine(staged, "staged")
	} else if staged > 0 {
		meta += fmt.Sprintf(" · %d staged", staged)
	}
	if adds+dels > 0 {
		meta += "  " + m.th.EditAdd.Render(fmt.Sprintf("+%d", adds)) + " " + m.th.EditDel.Render(fmt.Sprintf("-%d", dels))
	}
	title := m.sess.Title
	if title == "" {
		title = "this session"
	}
	header := " " + m.th.Title.Render("review") + m.th.Faint.Render("  ──  "+truncate(title, m.w/3)+"  ──  ") + meta

	bodyH := m.h - 3
	if bodyH < 1 {
		bodyH = 1
	}
	tree := m.viewReviewTree(bodyH)
	var right string
	if m.rvDraft {
		right = m.viewDraft(bodyH)
	} else {
		right = m.rvVP.View()
		if len(m.diff) == 0 && !m.diffLoading {
			right = "\n " + m.th.Faint.Render("nothing to review — the tree matches the baseline")
		}
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, tree, right)

	return header + "\n" + body + "\n" + m.viewReviewFooter()
}

func (m *model) viewReviewTree(height int) string {
	w := m.rvTreeWidth()
	rows := make([]string, 0, len(m.diff)+2)
	sel := func(i int, s string) string {
		if i == m.rvSel && !m.rvFocus {
			return m.th.ListSel.Render("> ") + s
		}
		return "  " + s
	}
	rows = append(rows, sel(0, m.th.EditPath.Render("all changes")))
	for i, f := range m.diff {
		letter := m.th.Brain.Render("M")
		switch f.Status {
		case "A":
			letter = m.th.EditAdd.Render("A")
		case "D":
			letter = m.th.EditDel.Render("D")
		}
		name := truncate(f.Path, w-9)
		if f.Outside {
			name += " " + m.th.ToolBad.Render("!")
		}
		rows = append(rows, sel(i+1, m.rvMarker(f)+" "+letter+" "+name))
	}
	col := lipgloss.NewStyle().Width(w).Height(height).Render(strings.Join(rows, "\n"))
	return col
}

func (m *model) viewDraft(height int) string {
	box := m.th.Composer.Width(m.w - m.rvTreeWidth() - 2).Render(
		m.th.EditPath.Bold(true).Render("commit") + "\n\n" + m.rvTA.View() + "\n\n" +
			m.th.StatusKey.Render("ctrl+d") + m.th.Faint.Render(" commit · ") +
			m.th.StatusKey.Render("esc") + m.th.Faint.Render(" back to review, staging kept"))
	return lipgloss.PlaceVertical(height, lipgloss.Top, box)
}

func (m *model) viewReviewFooter() string {
	if m.rvAskKeep {
		return " " + m.th.ToolBad.Render(m.keepQuestion())
	}
	if m.rvConfirm != "" {
		q := "Undo everything this session did? y/n"
		if m.rvConfirm != "*" {
			q = fmt.Sprintf("Put %s back to how it was before the session? y/n", m.rvConfirm)
			if f, ok := m.rvFile(); ok && f.Path == m.rvConfirm && f.Outside {
				q = fmt.Sprintf("You changed %s after the agent did. Overwrite your version with the pre-session one? y/n", m.rvConfirm)
			}
		}
		return " " + m.th.ToolBad.Render(q)
	}
	hints := " " + m.th.StatusKey.Render("a") + m.th.Faint.Render(" stage · ") +
		m.th.StatusKey.Render("A") + m.th.Faint.Render(" stage+commit · ") +
		m.th.StatusKey.Render("d") + m.th.Faint.Render(" restore · ") +
		m.th.StatusKey.Render("u") + m.th.Faint.Render(" undo all · ") +
		m.th.StatusKey.Render("r") + m.th.Faint.Render(" request changes · ") +
		m.th.StatusKey.Render("s") + m.th.Faint.Render(" plain summary · ") +
		m.th.StatusKey.Render("q") + m.th.Faint.Render(" back")
	if m.rvStatus != "" {
		status := m.rvStatus
		if strings.HasPrefix(status, "×") {
			status = m.th.ToolBad.Render(status)
		} else {
			status = m.th.Finding.Render(status)
		}
		return " " + status + m.th.Faint.Render("  ·") + hints
	}
	return hints
}
