// Package tui is the terminal client. It talks to the hive through
// hive.Service and renders waggle events; it knows nothing about
// adapters, journals, or the engine.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

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
	p := tea.NewProgram(newModel(svc, opts))
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
	sess     hive.SessionInfo
	draft    bool // no session created yet; first send creates one
	items    []*item
	byRef    map[string]int // bee/ref -> items index, for in-place card updates
	lastSeq  uint64
	vp       viewport.Model
	ta       textarea.Model
	spin     spinner.Model
	working  bool
	steering bool // turn stopped by esc, next send resumes
	started  time.Time
	verb     string      // this turn's working verb
	tokens   waggle.Cost // settled usage, accumulated across turns
	live     waggle.Cost // running usage of the current turn, replaced by pulses
	expanded bool        // tool output expanded
	errText  string
	watch    <-chan waggle.Event
	cancel   context.CancelFunc

	// list
	sessions []hive.SessionInfo
	cursor   int
}

func newModel(svc hive.Service, opts Options) *model {
	ta := textarea.New()
	ta.Placeholder = "what should we build?"
	ta.Prompt = ""
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.DynamicHeight = true
	ta.MinHeight = 1
	ta.MaxHeight = 6
	ta.SetVirtualCursor(true)
	ta.KeyMap.InsertNewline.SetEnabled(false)

	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))

	m := &model{
		svc: svc, opts: opts,
		th: newTheme(true), md: newMDCache(true),
		screen: screenChat, draft: true,
		byRef: map[string]int{},
		ta:    ta, spin: sp, vp: viewport.New(),
	}
	m.applyTheme()
	return m
}

func (m *model) applyTheme() {
	m.spin.Style = m.th.Brain
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
	send  string // message to send once the session is applied (draft flow)
}
type sentMsg struct{}
type stoppedMsg struct{}
type tickMsg time.Time

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, m.ta.Focus(), tea.RequestBackgroundColor)
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

// openSession creates or resumes a session and starts watching it. A
// non-empty send is delivered after the open lands in Update, never
// before, so a draft's first message cannot race the session id.
func (m *model) openSession(id waggle.SessionID, send string) tea.Cmd {
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
		return openedMsg{info: info, watch: ch, stop: stop, send: send}
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

func (m *model) stopTurn() tea.Cmd {
	id := m.sess.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := m.svc.Stop(ctx, id); err != nil {
			return errMsg{err}
		}
		return stoppedMsg{}
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

	case tea.BackgroundColorMsg:
		dark := msg.IsDark()
		if dark != m.th.dark {
			m.th = newTheme(dark)
			m.md = newMDCache(dark)
			m.applyTheme()
			m.redrawAll()
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.key(msg)

	case openedMsg:
		if m.cancel != nil {
			m.cancel()
		}
		m.sess, m.watch, m.cancel = msg.info, msg.watch, msg.stop
		m.draft = false
		m.resetTranscript()
		m.screen = screenChat
		m.refresh(true)
		if msg.send != "" {
			return m, tea.Batch(waitEvent(m.watch), m.send(msg.send))
		}
		return m, waitEvent(m.watch)

	case eventMsg:
		m.apply(msg.ev)
		return m, waitEvent(m.watch)

	case watchDoneMsg:
		return m, nil

	case sentMsg:
		m.working = true
		m.steering = false
		m.started = time.Now()
		m.verb = pickVerb(m.started)
		m.live = waggle.Cost{}
		m.errText = ""
		return m, tick()

	case stoppedMsg:
		m.working = false
		m.steering = true
		m.finishRunningCards()
		return m, nil

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
		if m.hasLiveCards() {
			m.refresh(m.vp.AtBottom())
		}
		return m, cmd
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

func (m *model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.screen == screenList {
		return m.keyList(msg)
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
	case "ctrl+o":
		m.expanded = !m.expanded
		m.unfreezeTools()
		m.refresh(m.vp.AtBottom())
		return m, nil
	case "pgup":
		m.vp.PageUp()
		return m, nil
	case "pgdown":
		m.vp.PageDown()
		return m, nil
	case "esc":
		if m.working {
			return m, m.stopTurn()
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
		m.layout()
		if m.draft {
			return m, m.openSession("", text)
		}
		return m, m.send(text)
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.layout()
	return m, cmd
}

func (m *model) keyList(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
			return m, m.openSession(m.sessions[m.cursor].ID, "")
		}
		return m, nil
	}
	return m, nil
}

func (m *model) startDraft() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.sess = hive.SessionInfo{}
	m.draft = true
	m.resetTranscript()
	m.screen = screenChat
	m.refresh(true)
}

func (m *model) resetTranscript() {
	m.items = nil
	m.byRef = map[string]int{}
	m.lastSeq = 0
	m.tokens = waggle.Cost{}
	m.live = waggle.Cost{}
	m.errText = ""
	m.working = false
	m.steering = false
}

// verbs are what the hive is doing while a turn runs. One is picked per
// turn so the working line has some life without being noisy.
var verbs = []string{"Buzzing", "Foraging", "Waggling", "Humming", "Pollinating", "Combing", "Swarming", "Dancing"}

func pickVerb(t time.Time) string {
	return verbs[int(t.UnixNano()/1000)%len(verbs)]
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
			if c.Live {
				// A pulse carries the whole turn so far; replace, never add.
				m.live = c
				return
			}
			m.tokens.InputTokens += c.InputTokens
			m.tokens.CachedInputTokens += c.CachedInputTokens
			m.tokens.OutputTokens += c.OutputTokens
			m.tokens.ReasoningTokens += c.ReasoningTokens
			m.live = waggle.Cost{}
		}
		return
	case waggle.KindResult:
		m.working = false
		m.live = waggle.Cost{}
		m.finishRunningCards()
		m.refresh(m.vp.AtBottom())
		return
	case waggle.KindDied:
		m.working = false
		m.live = waggle.Cost{}
	case waggle.KindMessage:
		if ev.Bee == "human" {
			m.working = true
			m.steering = false
			if m.started.IsZero() {
				m.started = time.Now()
			}
			if m.verb == "" {
				m.verb = pickVerb(m.started)
			}
		}
	case waggle.KindTool, waggle.KindEdit, waggle.KindPlan:
		if idx, ok := m.byRef[refKey(ev)]; ok {
			prev := m.items[idx]
			prev.ev = ev
			prev.frozen = false
			m.refresh(m.vp.AtBottom())
			return
		}
	}
	it := &item{ev: ev, start: ev.At}
	m.items = append(m.items, it)
	if k := refKey(ev); k != "" {
		m.byRef[k] = len(m.items) - 1
	}
	m.refresh(m.vp.AtBottom() || m.vp.PastBottom())
}

func refKey(ev waggle.Event) string {
	switch ev.Kind {
	case waggle.KindTool:
		if t, ok := decode[waggle.Tool](ev.Data); ok && t.Ref != "" {
			return ev.Bee + "/" + t.Ref
		}
	case waggle.KindEdit:
		if e, ok := decode[waggle.Edit](ev.Data); ok && e.Ref != "" {
			return ev.Bee + "/" + e.Ref
		}
	case waggle.KindPlan:
		if p, ok := decode[waggle.Plan](ev.Data); ok && p.Ref != "" {
			return ev.Bee + "/" + p.Ref
		}
	}
	return ""
}

// finishRunningCards marks any still-running card final so it stops
// animating after the turn ends (stop, result, or death).
func (m *model) finishRunningCards() {
	for _, it := range m.items {
		it.frozen = true
	}
}

func (m *model) unfreezeTools() {
	for _, it := range m.items {
		if it.ev.Kind == waggle.KindTool {
			it.frozen = false
		}
	}
}

func (m *model) redrawAll() {
	for _, it := range m.items {
		it.frozen = false
	}
	m.refresh(m.vp.AtBottom())
}

func (m *model) hasLiveCards() bool {
	for _, it := range m.items {
		if !it.frozen {
			return true
		}
	}
	return false
}

// --- view ---

func (m *model) layout() {
	if m.w == 0 {
		return
	}
	m.ta.SetWidth(m.w - 4) // composer border + padding
	m.vp.SetWidth(m.w)
	h := m.h - m.ta.Height() - 6 // header + activity + composer box + status
	if h < 1 {
		h = 1
	}
	m.vp.SetHeight(h)
}

func (m *model) refresh(follow bool) {
	if m.w == 0 {
		return
	}
	rc := renderCtx{
		th: m.th, md: m.md, width: m.vp.Width(),
		frame: m.spin.View(), expanded: m.expanded, now: time.Now(),
	}
	var b strings.Builder
	first := true
	for _, it := range m.items {
		s := it.render(rc)
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

func (m *model) View() tea.View {
	var content string
	switch {
	case m.w == 0:
		content = "loading…"
	case m.screen == screenList:
		content = m.viewList()
	default:
		content = m.viewChat()
	}
	v := tea.NewView(content)
	v.AltScreen = true
	v.WindowTitle = "hachi"
	return v
}

func (m *model) viewChat() string {
	body := m.vp.View()
	if len(m.items) == 0 {
		body = m.viewWelcome()
	}
	composer := m.th.Composer.Width(m.w - 2).Render(m.ta.View())
	return m.viewHeader() + "\n" + body + "\n" + m.viewActivity() + "\n" + composer + "\n" + m.viewStatus()
}

// viewActivity is the line above the composer: alive while a turn runs,
// with elapsed time and tokens streaming in; the steer prompt after esc.
func (m *model) viewActivity() string {
	switch {
	case m.working:
		s := " " + m.spin.View() + " " + m.th.Brain.Render(m.verb+"…")
		parts := []string{time.Since(m.started).Round(time.Second).String()}
		if out := m.live.OutputTokens + m.live.ReasoningTokens; out > 0 {
			parts = append(parts, "↓ "+compact(out)+" tokens")
		}
		parts = append(parts, "esc to interrupt")
		return s + m.th.Faint.Render(" ("+strings.Join(parts, " · ")+")")
	case m.steering:
		return " " + m.th.StatusKey.Render("interrupted") + m.th.Faint.Render(" · tell the hive where to go next")
	}
	return ""
}

func (m *model) viewHeader() string {
	left := " " + m.th.Header.Render("hachi") + " " + m.th.HeaderSub.Render("蜂")
	if t := m.sess.Title; t != "" {
		left += m.th.HeaderSub.Render("  ·  ") + m.th.HeaderSub.Render(truncate(t, m.w/2))
	}
	right := m.th.BrainChip.Render(m.brainName()) + " "
	gap := m.w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m *model) viewWelcome() string {
	logo := m.th.Logo.Render("hachi 蜂")
	sub := m.th.Faint.Render("the glass hive")
	where := m.th.Faint.Render(m.opts.Dir)
	hints := m.th.StatusKey.Render("enter") + m.th.Faint.Render(" send · ") +
		m.th.StatusKey.Render("esc") + m.th.Faint.Render(" stop · ") +
		m.th.StatusKey.Render("ctrl+l") + m.th.Faint.Render(" sessions")
	block := lipgloss.JoinVertical(lipgloss.Center, logo, "", sub, where, "", hints)
	return lipgloss.Place(m.w, m.vp.Height(), lipgloss.Center, lipgloss.Center, block)
}

func (m *model) viewStatus() string {
	var left string
	switch {
	case m.errText != "":
		left = " " + m.th.ToolBad.Render("× "+truncate(m.errText, m.w/2))
	case m.working:
		left = " " + m.th.Brain.Render("●") + m.th.StatusBar.Render(" working")
	default:
		left = " " + m.th.Faint.Render("○") + m.th.StatusBar.Render(" idle")
	}
	in := m.tokens.InputTokens + m.live.InputTokens
	out := m.tokens.OutputTokens + m.tokens.ReasoningTokens + m.live.OutputTokens + m.live.ReasoningTokens
	if in+out > 0 {
		left += m.th.StatusBar.Render(fmt.Sprintf("  ·  ↑ %s ↓ %s", compact(in), compact(out)))
		if cached := m.tokens.CachedInputTokens + m.live.CachedInputTokens; cached > 0 && in > 0 {
			left += m.th.StatusBar.Render(fmt.Sprintf(" · %d%% cached", cached*100/in))
		}
	}
	help := m.th.StatusKey.Render("enter") + m.th.Faint.Render(" send  ") +
		m.th.StatusKey.Render("esc") + m.th.Faint.Render(" stop  ") +
		m.th.StatusKey.Render("^o") + m.th.Faint.Render(" expand  ") +
		m.th.StatusKey.Render("^l") + m.th.Faint.Render(" sessions  ") +
		m.th.StatusKey.Render("^c") + m.th.Faint.Render(" quit ")
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
		row := fmt.Sprintf("%s  %s  %s", stateDot(m.th, s.State), truncate(title, m.w-30),
			m.th.Faint.Render(s.Updated.Format("Jan 2 15:04")))
		if i == m.cursor {
			row = m.th.ListSel.Render("→ ") + row
		} else {
			row = "  " + row
		}
		b.WriteString(row + "\n")
	}
	b.WriteString("\n" + m.th.Faint.Render("enter open · n new · esc back"))
	panel := m.th.ListBox.Render(b.String())
	return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, panel)
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
