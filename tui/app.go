// Package tui is the terminal client. It talks to the hive through
// hive.Service and renders waggle events; it knows nothing about
// adapters, journals, or the engine.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

// Options configure the client; the command layer fills these in.
type Options struct {
	Dir    string // working directory for new sessions
	Brain  string // adapter name for new sessions
	Brains []string
}

// Run starts the TUI and blocks until the user quits.
func Run(svc hive.Service, opts Options) error {
	m := newModel(svc, opts)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

type screen int

const (
	screenChat screen = iota
	screenList
)

type model struct {
	svc  hive.Service
	opts Options
	th   theme
	md   *mdCache

	w, h   int
	screen screen

	// chat
	sess    hive.SessionInfo
	draft   bool // no session created yet; first send creates one
	items   []*item
	lastSeq uint64
	vp      viewport.Model
	ta      textarea.Model
	spin    spinner.Model
	working bool
	started time.Time
	tokens  waggle.Cost
	errText string
	watch   <-chan waggle.Event
	cancel  context.CancelFunc

	// list
	sessions []hive.SessionInfo
	cursor   int
}

func newModel(svc hive.Service, opts Options) *model {
	th := newTheme()

	ta := textarea.New()
	ta.Placeholder = "what should we build? (enter sends, ctrl+j newline)"
	ta.Prompt = "┃ "
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.SetHeight(1)
	ta.KeyMap.InsertNewline.SetEnabled(false)
	ta.Focus()

	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	sp.Style = th.Brain

	return &model{
		svc: svc, opts: opts, th: th, md: newMDCache(),
		screen: screenChat, draft: true,
		ta: ta, spin: sp, vp: viewport.New(0, 0),
	}
}

// --- messages ---

type eventMsg struct{ ev waggle.Event }
type watchDoneMsg struct{}
type errMsg struct{ err error }
type sessionsMsg struct{ list []hive.SessionInfo }
type openedMsg struct {
	info  hive.SessionInfo
	watch <-chan waggle.Event
	stop  context.CancelFunc
}
type sentMsg struct{}
type tickMsg time.Time

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, textarea.Blink)
}

func waitEvent(ch <-chan waggle.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return watchDoneMsg{}
		}
		return eventMsg{ev: ev}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) loadSessions() tea.Cmd {
	return func() tea.Msg {
		list, err := m.svc.Sessions(context.Background())
		if err != nil {
			return errMsg{err}
		}
		return sessionsMsg{list}
	}
}

// openSession creates or resumes a session and starts watching it.
func (m *model) openSession(id waggle.SessionID) tea.Cmd {
	dir, brain := m.opts.Dir, m.opts.Brain
	return func() tea.Msg {
		info, err := m.svc.Open(context.Background(), id, dir, brain)
		if err != nil {
			return errMsg{err}
		}
		ctx, stop := context.WithCancel(context.Background())
		ch, err := m.svc.Watch(ctx, info.ID)
		if err != nil {
			stop()
			return errMsg{err}
		}
		return openedMsg{info: info, watch: ch, stop: stop}
	}
}

func (m *model) send(text string) tea.Cmd {
	id := m.sess.ID
	return func() tea.Msg {
		if err := m.svc.Send(context.Background(), id, text); err != nil {
			return errMsg{err}
		}
		return sentMsg{}
	}
}

// --- update ---

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.layout()
		m.refresh(true)
		return m, nil

	case tea.KeyMsg:
		return m.key(msg)

	case openedMsg:
		if m.cancel != nil {
			m.cancel()
		}
		m.sess, m.watch, m.cancel = msg.info, msg.watch, msg.stop
		m.draft = false
		m.items = nil
		m.lastSeq = 0
		m.tokens = waggle.Cost{}
		m.errText = ""
		m.screen = screenChat
		m.refresh(true)
		return m, waitEvent(m.watch)

	case eventMsg:
		m.apply(msg.ev)
		return m, waitEvent(m.watch)

	case watchDoneMsg:
		return m, nil

	case sentMsg:
		m.working = true
		m.started = time.Now()
		m.errText = ""
		return m, tick()

	case sessionsMsg:
		m.sessions = msg.list
		if m.cursor >= len(m.sessions) {
			m.cursor = 0
		}
		return m, nil

	case errMsg:
		m.errText = msg.err.Error()
		m.working = false
		return m, nil

	case tickMsg:
		if m.working {
			return m, tick()
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

func (m *model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.screen == screenList {
		switch msg.String() {
		case "ctrl+c", "q", "esc", "ctrl+l":
			m.screen = screenChat
			return m, nil
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case "down", "j":
			if m.cursor < len(m.sessions)-1 {
				m.cursor++
			}
			return m, nil
		case "n":
			m.startDraft()
			return m, nil
		case "enter":
			if len(m.sessions) > 0 {
				return m, m.openSession(m.sessions[m.cursor].ID)
			}
			return m, nil
		}
		return m, nil
	}

	switch msg.String() {
	case "ctrl+c":
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	case "ctrl+l":
		m.screen = screenList
		return m, m.loadSessions()
	case "ctrl+n":
		m.startDraft()
		return m, nil
	case "esc":
		if m.working {
			id := m.sess.ID
			return m, func() tea.Msg {
				if err := m.svc.Stop(context.Background(), id); err != nil {
					return errMsg{err}
				}
				return nil
			}
		}
		return m, nil
	case "ctrl+j":
		m.ta.InsertString("\n")
		m.layout()
		return m, nil
	case "enter":
		text := strings.TrimSpace(m.ta.Value())
		if text == "" {
			return m, nil
		}
		if m.working {
			m.errText = "a turn is running; esc stops it first"
			return m, nil
		}
		m.ta.Reset()
		m.ta.SetHeight(1)
		m.layout()
		if m.draft {
			return m, tea.Sequence(m.openSession(""), m.sendAfterOpen(text))
		}
		return m, m.send(text)
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.layout()
	return m, cmd
}

// sendAfterOpen fires after openSession has set m.sess; tea.Sequence
// guarantees the ordering.
func (m *model) sendAfterOpen(text string) tea.Cmd {
	return func() tea.Msg {
		if m.sess.ID == "" {
			return errMsg{fmt.Errorf("session not ready")}
		}
		if err := m.svc.Send(context.Background(), m.sess.ID, text); err != nil {
			return errMsg{err}
		}
		return sentMsg{}
	}
}

func (m *model) startDraft() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.sess = hive.SessionInfo{}
	m.draft = true
	m.items = nil
	m.lastSeq = 0
	m.tokens = waggle.Cost{}
	m.errText = ""
	m.working = false
	m.screen = screenChat
	m.refresh(true)
}

// apply folds one event into the transcript and status.
func (m *model) apply(ev waggle.Event) {
	if ev.Seq != 0 && ev.Seq <= m.lastSeq {
		return
	}
	m.lastSeq = ev.Seq
	switch ev.Kind {
	case waggle.KindCost:
		if c, ok := decode[waggle.Cost](ev.Data); ok {
			m.tokens.InputTokens += c.InputTokens
			m.tokens.CachedInputTokens += c.CachedInputTokens
			m.tokens.OutputTokens += c.OutputTokens
			m.tokens.ReasoningTokens += c.ReasoningTokens
		}
		return
	case waggle.KindResult:
		m.working = false
		return
	case waggle.KindDied:
		m.working = false
	case waggle.KindMessage:
		if ev.Bee == "human" {
			m.working = true
			if m.started.IsZero() {
				m.started = time.Now()
			}
		}
	}
	m.items = append(m.items, &item{ev: ev})
	m.refresh(m.vp.AtBottom() || m.vp.PastBottom())
}

// --- view ---

func (m *model) layout() {
	if m.w == 0 {
		return
	}
	lines := strings.Count(m.ta.Value(), "\n") + 1
	if lines > 6 {
		lines = 6
	}
	m.ta.SetHeight(lines)
	m.ta.SetWidth(m.w - 2)
	m.vp.Width = m.w
	m.vp.Height = m.h - m.ta.Height() - 3 // composer + status + gap
	if m.vp.Height < 1 {
		m.vp.Height = 1
	}
}

func (m *model) refresh(follow bool) {
	if m.w == 0 {
		return
	}
	var b strings.Builder
	first := true
	for _, it := range m.items {
		s := it.render(m.th, m.md, m.vp.Width)
		if s == "" {
			continue
		}
		if !first {
			b.WriteString("\n\n")
		}
		first = false
		b.WriteString(s)
	}
	m.vp.SetContent(b.String())
	if follow {
		m.vp.GotoBottom()
	}
}

func (m *model) View() string {
	if m.w == 0 {
		return "loading…"
	}
	if m.screen == screenList {
		return m.viewList()
	}
	body := m.vp.View()
	if len(m.items) == 0 {
		body = m.viewWelcome()
	}
	return body + "\n" + m.ta.View() + "\n" + m.viewStatus()
}

func (m *model) viewWelcome() string {
	logo := m.th.Logo.Render("hachi 蜂")
	sub := m.th.Faint.Render("the glass hive · brain: " + m.brainName() + " · dir: " + m.opts.Dir)
	block := lipgloss.JoinVertical(lipgloss.Center, logo, "", sub)
	return lipgloss.Place(m.w, m.vp.Height, lipgloss.Center, lipgloss.Center, block)
}

func (m *model) viewStatus() string {
	left := m.th.Brain.Render(m.brainName())
	switch {
	case m.errText != "":
		left += "  " + m.th.ToolBad.Render(truncate(m.errText, m.w/2))
	case m.working:
		left += "  " + m.spin.View() + m.th.StatusBar.Render(
			fmt.Sprintf(" working %s", time.Since(m.started).Round(time.Second)))
	default:
		left += "  " + m.th.StatusBar.Render("idle")
	}
	if n := m.tokens.InputTokens + m.tokens.OutputTokens; n > 0 {
		left += m.th.StatusBar.Render(fmt.Sprintf("  %s tok", compact(n)))
	}
	help := m.th.StatusBar.Render("enter") + m.th.Faint.Render(" send  ") +
		m.th.StatusBar.Render("esc") + m.th.Faint.Render(" stop  ") +
		m.th.StatusBar.Render("^l") + m.th.Faint.Render(" sessions  ") +
		m.th.StatusBar.Render("^n") + m.th.Faint.Render(" new  ") +
		m.th.StatusBar.Render("^c") + m.th.Faint.Render(" quit")
	gap := m.w - lipgloss.Width(left) - lipgloss.Width(help)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + help
}

func (m *model) viewList() string {
	var b strings.Builder
	b.WriteString(m.th.Title.Render("sessions") + "\n\n")
	if len(m.sessions) == 0 {
		b.WriteString(m.th.Faint.Render("nothing yet — press n to start one"))
	}
	for i, s := range m.sessions {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		row := fmt.Sprintf("%s  %s  %s", stateDot(m.th, s.State), title,
			m.th.Faint.Render(s.Updated.Format("Jan 2 15:04")))
		if i == m.cursor {
			row = m.th.ListSel.Render("→ ") + row
		} else {
			row = "  " + row
		}
		b.WriteString(row + "\n")
	}
	b.WriteString("\n" + m.th.Faint.Render("enter open · n new · esc back"))
	return lipgloss.NewStyle().Padding(1, 2).Render(b.String())
}

func stateDot(t theme, s hive.State) string {
	switch s {
	case hive.StateWorking:
		return t.Brain.Render("●")
	case hive.StateNeeds:
		return t.ToolBad.Render("●")
	case hive.StateDied:
		return t.ToolBad.Render("✗")
	default:
		return t.Faint.Render("○")
	}
}

func (m *model) brainName() string {
	if m.sess.Brain != "" {
		return m.sess.Brain
	}
	return m.opts.Brain
}

func compact(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
