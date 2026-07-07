package tui

// The board is a session switcher, one tab away: four columns derived
// from the same session list the strip counts, so the two can never
// disagree. Nothing here is stored; columns are recomputed from
// m.sessions on every render, and the only board state is the cursor.
// You never file a card, you never drag one: opening a session creates
// it, its real state moves it, closing the session removes it.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

const (
	colWorking = iota
	colNeeds
	colReview
	colDone
)

var colNames = [4]string{"working", "needs you", "review", "done"}

// column places one session, the whole truth table. Anything raised is
// needs you; a queued session counts as working because a turn is on
// the way; review is the parked diff; everything else has settled.
func column(s hive.SessionInfo) int {
	switch {
	case s.State == hive.StateNeeds || s.State == hive.StateDied:
		return colNeeds
	case s.State == hive.StateWorking || s.State == hive.StateWaiting:
		return colWorking
	case s.DiffReady:
		return colReview
	}
	return colDone
}

// boardColumns sorts every known session into its column. Needs-you
// orders by reason priority then oldest raise, the triage order; the
// rest by freshest last event, the way you think of them.
func (m *model) boardColumns() [4][]hive.SessionInfo {
	var cols [4][]hive.SessionInfo
	for _, s := range m.sessions {
		c := column(s)
		cols[c] = append(cols[c], s)
	}
	sort.SliceStable(cols[colNeeds], func(i, j int) bool {
		a, b := cols[colNeeds][i], cols[colNeeds][j]
		if ra, rb := needRank(a.Reason), needRank(b.Reason); ra != rb {
			return ra < rb
		}
		return a.Raised.Before(b.Raised)
	})
	for _, c := range []int{colWorking, colReview, colDone} {
		sort.SliceStable(cols[c], func(i, j int) bool {
			return cols[c][i].Updated.After(cols[c][j].Updated)
		})
	}
	return cols
}

// needRank orders raised reasons: something wrong beats something
// merely done. Shared with the strip's named reason.
func needRank(r string) int {
	switch r {
	case "question":
		return 0
	case "died":
		return 1
	case "stall":
		return 2
	case "diff":
		return 3
	}
	return 4
}

// openBoard enters the board. The cursor jumps to the top of needs you
// when anything is raised, because that is what you came to triage;
// otherwise it stays where you last left it.
func (m *model) openBoard() tea.Cmd {
	m.screen = screenBoard
	cols := m.boardColumns()
	if len(cols[colNeeds]) > 0 {
		m.bdCol, m.bdRow = colNeeds, 0
	} else if m.bdRow >= len(cols[m.bdCol]) {
		m.bdRow = max(0, len(cols[m.bdCol])-1)
	}
	return m.loadSessions()
}

func (m *model) keyBoard(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.delAsk != "" {
		return m.keyDeleteAsk(msg)
	}
	m.note = ""
	cols := m.boardColumns()
	clamp := func() {
		if m.bdRow >= len(cols[m.bdCol]) {
			m.bdRow = max(0, len(cols[m.bdCol])-1)
		}
	}
	switch msg.String() {
	case "ctrl+c":
		m.quitAll()
		return m, tea.Quit
	case "tab", "q", "esc":
		m.screen = screenChat
		return m, nil
	case "ctrl+l":
		m.screen = screenList
		return m, m.loadSessions()
	case "h", "left":
		if m.bdCol > 0 {
			m.bdCol--
			clamp()
		}
		return m, nil
	case "l", "right":
		if m.bdCol < 3 {
			m.bdCol++
			clamp()
		}
		return m, nil
	case "k", "up":
		if m.bdRow > 0 {
			m.bdRow--
		}
		return m, nil
	case "j", "down":
		if m.bdRow < len(cols[m.bdCol])-1 {
			m.bdRow++
		}
		return m, nil
	case "n", "ctrl+n":
		return m, m.startDraft()
	case "x":
		if m.bdRow < len(cols[m.bdCol]) {
			m.delAsk = cols[m.bdCol][m.bdRow].ID
		}
		return m, nil
	case "enter":
		if m.bdRow >= len(cols[m.bdCol]) {
			return m, nil
		}
		id := cols[m.bdCol][m.bdRow].ID
		if v, ok := m.open[id]; ok {
			return m, m.focus(v)
		}
		return m, m.openSession(id, "")
	}
	return m, nil
}

// --- render ---

// viewBoard draws the whole screen: four columns side by side, or one
// at a time below 100 columns with the header row as the navigator.
func (m *model) viewBoard() string {
	cols := m.boardColumns()
	if m.w < 100 {
		return m.viewBoardNarrow(cols)
	}

	cw := min(40, max(22, m.w/4-4))
	bodyH := m.h - 3 - m.stripRows() // title + header gap + footer
	perCol := max(1, (bodyH-2)/5)    // header rows, then 5 rows a card

	rendered := make([]string, 0, 4)
	for c := range cols {
		head := colNames[c] + " " + fmt.Sprint(len(cols[c]))
		style := m.th.Faint
		if len(cols[c]) > 0 {
			style = m.th.Title
		}
		lines := []string{" " + style.Render(head), " " + m.th.Faint.Render(strings.Repeat("─", lipgloss.Width(head)))}
		for i, s := range m.visibleCards(cols[c], c, perCol) {
			lines = append(lines, m.viewCard(s, cw, c == m.bdCol && m.cardIndex(cols[c], i, perCol) == m.bdRow))
		}
		if extra := len(cols[c]) - perCol; extra > 0 {
			lines = append(lines, " "+m.th.Faint.Render(fmt.Sprintf("+%d more", extra)))
		}
		rendered = append(rendered, lipgloss.NewStyle().Width(cw+4).Render(strings.Join(lines, "\n")))
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, rendered...)
	return " " + m.th.Title.Render("board") + "\n" + body + "\n" + m.boardFooter()
}

// visibleCards windows a column around the cursor so the selected card
// is always on screen; the +N more line owns whatever falls off.
func (m *model) visibleCards(col []hive.SessionInfo, c, perCol int) []hive.SessionInfo {
	if len(col) <= perCol {
		return col
	}
	start := 0
	if c == m.bdCol && m.bdRow >= perCol {
		start = m.bdRow - perCol + 1
	}
	return col[start:min(start+perCol, len(col))]
}

// cardIndex maps a visible slot back to its column index, undoing the
// scroll offset visibleCards applied.
func (m *model) cardIndex(col []hive.SessionInfo, slot, perCol int) int {
	if len(col) <= perCol || m.bdCol < 0 {
		return slot
	}
	start := 0
	if m.bdRow >= perCol {
		start = m.bdRow - perCol + 1
	}
	return start + slot
}

func (m *model) viewBoardNarrow(cols [4][]hive.SessionInfo) string {
	var heads []string
	for c := range cols {
		head := fmt.Sprintf("%s %d", colNames[c], len(cols[c]))
		if c == m.bdCol {
			heads = append(heads, m.th.Title.Render("["+head+"]"))
		} else {
			heads = append(heads, m.th.Faint.Render(head))
		}
	}
	lines := []string{" " + m.th.Title.Render("board"), " " + strings.Join(heads, "  ")}
	cw := m.w - 6
	bodyH := m.h - 4 - m.stripRows()
	perCol := max(1, bodyH/5)
	col := cols[m.bdCol]
	for i, s := range m.visibleCards(col, m.bdCol, perCol) {
		lines = append(lines, m.viewCard(s, cw, m.cardIndex(col, i, perCol) == m.bdRow))
	}
	if extra := len(col) - perCol; extra > 0 {
		lines = append(lines, " "+m.th.Faint.Render(fmt.Sprintf("+%d more", extra)))
	}
	return strings.Join(lines, "\n") + "\n" + m.boardFooter()
}

func (m *model) boardFooter() string {
	if m.delAsk != "" {
		return " " + m.th.ToolBad.Render(m.deleteAskLine())
	}
	if m.note != "" {
		return " " + m.th.Faint.Render(m.note)
	}
	return " " + m.th.StatusKey.Render("h/l") + m.th.Faint.Render(" column · ") +
		m.th.StatusKey.Render("j/k") + m.th.Faint.Render(" card · ") +
		m.th.StatusKey.Render("enter") + m.th.Faint.Render(" open · ") +
		m.th.StatusKey.Render("x") + m.th.Faint.Render(" delete · ") +
		m.th.StatusKey.Render("ctrl+n") + m.th.Faint.Render(" new · ") +
		m.th.StatusKey.Render("tab") + m.th.Faint.Render(" back")
}

// viewCard is the three-row face: glyph and title, brain and vitals,
// then the last action in plain words. A raised card wears the
// attention color on its border; the selected card wears the theme's.
func (m *model) viewCard(s hive.SessionInfo, w int, selected bool) string {
	title := s.Title
	if title == "" {
		title = "(untitled)"
	}
	inner := w - 4 // Width covers the border and padding; text gets the rest
	row1 := stateDot(m.th, s.State) + " " + truncate(title, inner-2)

	vitals := s.Brain
	if v, ok := m.open[s.ID]; ok {
		if tok := v.tokens.InputTokens + v.tokens.OutputTokens + v.tokens.ReasoningTokens; tok > 0 {
			vitals += " · " + compact(tok) + " tok"
		}
	}
	vitals += " · " + ageShort(time.Since(s.Updated))
	row2 := m.th.Faint.Render(truncate(vitals, inner))

	last := truncate(m.lastAction(s), inner)
	row3 := m.th.Faint.Render(last)
	border := m.th.BoardCard
	if s.State == hive.StateNeeds || s.State == hive.StateDied {
		row3 = m.th.ToolBad.Render(last)
		border = m.th.BoardNeed
	}
	if selected {
		border = m.th.BoardSel
	}
	return border.Width(w).Render(row1 + "\n" + row2 + "\n" + row3)
}

// lastAction is the card's third row: why it needs you when it does,
// otherwise the freshest meaningful event from the live view, and a
// plain state word when the session was never opened this run.
func (m *model) lastAction(s hive.SessionInfo) string {
	if s.Detail != "" {
		return s.Detail
	}
	if s.State == hive.StateWaiting {
		return "waiting for the folder"
	}
	if v, ok := m.open[s.ID]; ok {
		if t := lastEventLine(v); t != "" {
			return t
		}
	}
	switch {
	case s.State == hive.StateWorking:
		return "working"
	case s.State == hive.StateDied:
		return "stopped"
	case s.DiffReady:
		return "changes to review"
	case s.Committed && s.Branch != "":
		return "committed on " + s.Branch
	}
	return "no changes"
}

// lastEventLine says the newest meaningful event in a sentence fragment,
// per the wording table in the board spec.
func lastEventLine(v *sview) string {
	for i := len(v.items) - 1; i >= 0; i-- {
		ev := v.items[i].ev
		switch ev.Kind {
		case waggle.KindTool:
			if t, ok := decode[waggle.Tool](ev.Data); ok {
				cmd := firstLine(t.Command)
				if t.Status == "in_progress" {
					return "running " + cmd
				}
				if t.ExitCode != nil && *t.ExitCode != 0 {
					return fmt.Sprintf("ran %s (exit %d)", cmd, *t.ExitCode)
				}
				return "ran " + cmd
			}
		case waggle.KindEdit:
			if e, ok := decode[waggle.Edit](ev.Data); ok && len(e.Changes) > 0 {
				return "edited " + e.Changes[len(e.Changes)-1].Path
			}
		case waggle.KindMessage:
			if ev.Bee == "human" {
				continue
			}
			if msg, ok := decode[waggle.Message](ev.Data); ok {
				return firstLine(msg.Text)
			}
		case waggle.KindFinding:
			if msg, ok := decode[waggle.Message](ev.Data); ok {
				return firstLine(msg.Text)
			}
		case waggle.KindNeedInput:
			if ni, ok := decode[waggle.NeedInput](ev.Data); ok {
				return firstLine(ni.Prompt)
			}
		case waggle.KindDied:
			if d, ok := decode[waggle.Died](ev.Data); ok && d.Error != "" {
				return "stopped: " + firstLine(d.Error)
			}
			return "stopped"
		case waggle.KindResult:
			return "done"
		case waggle.KindSpawned:
			return "starting…"
		}
	}
	return ""
}

// ageShort is time since the last event in the coarse units a glance
// reads: 12s, 2m, 1h, 2d.
func ageShort(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
