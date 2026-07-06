// Package tui is the terminal client. It talks to the hive through
// hive.Service and renders waggle events; it knows nothing about
// adapters, journals, or the engine.
package tui

import (
	"context"
	"errors"
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
	screenDiff
	screenReview
)

// sview is one open session's client-side state: transcript, composer,
// scroll position, turn status. Every session opened this run keeps one,
// watch alive, so switching away costs nothing and switching back needs
// no replay.
type sview struct {
	sess     hive.SessionInfo
	draft    bool // no session created yet; first send creates one
	items    []*item
	byRef    map[string]int // bee/ref -> items index, for in-place card updates
	lastSeq  uint64
	vp       viewport.Model
	ta       textarea.Model
	working  bool
	waiting  bool   // message queued behind a busy folder; flips to working when it starts
	busyAsk  string // refused message pending the wait-or-cancel answer
	busyWith string // title of the session holding the folder, may be empty
	steering bool // turn stopped by esc, next send resumes
	started  time.Time
	verb     string       // this turn's working verb
	tokens   waggle.Cost  // settled usage, accumulated across turns
	pulse    waggle.Pulse // the brain's live vitals, each pulse replaces the last
	expanded bool         // tool output expanded
	errText  string
	watch    <-chan waggle.Event
	cancel   context.CancelFunc
}

type model struct {
	svc  hive.Service
	opts Options
	th   theme
	md   *mdCache

	w, h   int
	screen screen

	*sview                              // the focused session; never nil
	open    map[waggle.SessionID]*sview // every open session, focused included
	scratch *sview                      // the one draft view, reused by ctrl+n
	spin    spinner.Model
	ticking bool // a 1s tick loop is running for some working session

	// list
	sessions []hive.SessionInfo
	cursor   int

	// diff-so-far
	diff        []hive.FileDiff
	diffErr     string
	diffMarks   []int // content line where each file's section starts
	diffLoading bool
	dvp         viewport.Model

	// review
	rvSel     int             // 0 is the summary row, files start at 1
	rvFocus   bool            // diff pane focused; j/k scroll instead of moving the tree
	rvDraft   bool            // commit draft editor open
	rvConfirm string          // restore pending confirm: a path, or * for everything
	rvStatus  string          // last verb's outcome, shown in the footer
	rvMergeQ  string          // merge-back pending confirm: the question, empty when none
	rvGone    map[string]bool // restored this visit, drawn as ✗
	rvVP      viewport.Model
	rvTA      textarea.Model
	reqBanner string // composer banner after request-changes
	reqDiff   string // diff text riding along with the next message

	// sentence view: the same review state in plain words
	rvPlain   bool     // sentence rendering active
	rvChose   bool     // the user toggled views; stop re-defaulting on open
	rvBtn     int      // 0 keep, 1 undo
	rvAskKeep bool     // non-git keep confirm pending
	rvKeep    []string // paths behind the pending keep; nil is everything
	rvNote    string   // one-shot suffix for the next staged status
	keptOnce  bool     // the narrows-undo sentence already ran this session
}

func newSview() *sview {
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

	return &sview{draft: true, byRef: map[string]int{}, ta: ta, vp: viewport.New()}
}

func newModel(svc hive.Service, opts Options) *model {
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))

	rta := textarea.New()
	rta.Prompt = ""
	rta.CharLimit = 0
	rta.ShowLineNumbers = false
	rta.SetVirtualCursor(true)

	m := &model{
		svc: svc, opts: opts,
		th: newTheme(true), md: newMDCache(true),
		screen: screenChat,
		open:   map[waggle.SessionID]*sview{},
		rvGone: map[string]bool{},
		spin:   sp, dvp: viewport.New(), rvVP: viewport.New(),
		rvTA: rta,
	}
	m.scratch = newSview()
	m.sview = m.scratch
	m.dvp.FillHeight = true
	m.rvVP.FillHeight = true
	m.applyTheme()
	return m
}

func (m *model) applyTheme() {
	m.spin.Style = m.th.Brain
}

// --- messages ---

type eventMsg struct {
	id waggle.SessionID
	ev waggle.Event
}
type watchDoneMsg struct{ id waggle.SessionID }
type errMsg struct {
	id  waggle.SessionID // empty means the focused session owns it
	err error
}
type sessionsMsg struct{ list []hive.SessionInfo }
type openedMsg struct {
	info  hive.SessionInfo
	watch <-chan waggle.Event
	stop  context.CancelFunc
	send  string // message to send once the session is applied (draft flow)
}
type sentMsg struct{ id waggle.SessionID }
type folderBusyMsg struct {
	id   waggle.SessionID
	text string // the message Send refused
	with string // who holds the folder, may be empty
}
type queuedMsg struct{ id waggle.SessionID }
type infoMsg struct{ info hive.SessionInfo }
type stoppedMsg struct{ id waggle.SessionID }
type tickMsg time.Time

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, m.ta.Focus(), tea.RequestBackgroundColor)
}

func waitEvent(id waggle.SessionID, ch <-chan waggle.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return watchDoneMsg{id: id}
		}
		return eventMsg{id: id, ev: ev}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) loadSessions() tea.Cmd {
	return func() tea.Msg {
		list, err := m.svc.Sessions(context.Background())
		if err != nil {
			return errMsg{err: err}
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
			return errMsg{err: err}
		}
		ctx, stop := context.WithCancel(context.Background())
		ch, err := m.svc.Watch(ctx, info.ID)
		if err != nil {
			stop()
			return errMsg{err: err}
		}
		return openedMsg{info: info, watch: ch, stop: stop, send: send}
	}
}

// refreshInfo re-reads the focused session's info. The engine can change
// it after open: the first turn in a busy repo moves the session into a
// worktree, and the review header wants that branch.
func (m *model) refreshInfo() tea.Cmd {
	id := m.sess.ID
	if id == "" {
		return nil
	}
	return func() tea.Msg {
		info, err := m.svc.Open(context.Background(), id, "", "")
		if err != nil {
			return nil
		}
		return infoMsg{info: info}
	}
}

func (m *model) send(text string) tea.Cmd {
	id := m.sess.ID
	return func() tea.Msg {
		err := m.svc.Send(context.Background(), id, text)
		if busy, ok := errors.AsType[*hive.FolderBusyError](err); ok {
			// Not a failure: the folder has another writer and the
			// human decides between waiting and cancelling.
			return folderBusyMsg{id: id, text: text, with: busy.With}
		}
		if err != nil {
			return errMsg{id: id, err: err}
		}
		return sentMsg{id: id}
	}
}

// queueSend parks the refused message with the engine; the turn starts
// by itself once the folder frees up.
func (m *model) queueSend(text string) tea.Cmd {
	id := m.sess.ID
	return func() tea.Msg {
		if err := m.svc.Queue(context.Background(), id, text); err != nil {
			return errMsg{id: id, err: err}
		}
		return queuedMsg{id: id}
	}
}

func (m *model) stopTurn() tea.Cmd {
	id := m.sess.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := m.svc.Stop(ctx, id); err != nil {
			return errMsg{id: id, err: err}
		}
		return stoppedMsg{id: id}
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
		if v, ok := m.open[msg.info.ID]; ok {
			// Already open (a double enter); the duplicate watch goes.
			msg.stop()
			cmd := m.focus(v)
			if msg.send != "" {
				return m, tea.Batch(cmd, m.send(msg.send))
			}
			return m, cmd
		}
		v := newSview()
		if m.draft {
			// A draft turning real keeps its composer; the scratch view
			// is spent and ctrl+n builds the next one fresh.
			v.ta = m.ta
			if m.sview == m.scratch {
				m.scratch = nil
			}
		}
		v.sess, v.watch, v.cancel = msg.info, msg.watch, msg.stop
		v.draft = false
		m.open[msg.info.ID] = v
		cmd := m.focus(v)
		if msg.send != "" {
			return m, tea.Batch(cmd, waitEvent(v.sess.ID, v.watch), m.send(msg.send))
		}
		return m, tea.Batch(cmd, waitEvent(v.sess.ID, v.watch))

	case eventMsg:
		v := m.viewOf(msg.id)
		if v == nil {
			return m, nil
		}
		m.apply(v, msg.ev)
		if v.working && !m.ticking {
			// A queued turn starts without a sentMsg; the human message
			// arriving on the watch is its starting gun.
			m.ticking = true
			return m, tea.Batch(waitEvent(msg.id, v.watch), tick())
		}
		return m, waitEvent(msg.id, v.watch)

	case watchDoneMsg:
		return m, nil

	case sentMsg:
		v := m.viewOf(msg.id)
		if v == nil {
			return m, nil
		}
		v.working = true
		v.steering = false
		v.started = time.Now()
		v.verb = pickVerb(v.started)
		v.pulse.Usage = waggle.Cost{}
		v.errText = ""
		if m.ticking {
			return m, nil
		}
		m.ticking = true
		return m, tick()

	case folderBusyMsg:
		v := m.viewOf(msg.id)
		if v == nil {
			v = m.sview
		}
		v.busyAsk, v.busyWith = msg.text, msg.with
		v.working = false
		return m, nil

	case queuedMsg:
		if v := m.viewOf(msg.id); v != nil {
			v.waiting = true
			v.errText = ""
		}
		return m, nil

	case mergedMsg:
		return m, m.applyMerged(msg)

	case infoMsg:
		if v := m.viewOf(msg.info.ID); v != nil {
			v.sess = msg.info
			if m.screen == screenReview && v == m.sview {
				m.renderReview()
			}
		}
		return m, nil

	case stoppedMsg:
		v := m.viewOf(msg.id)
		if v == nil {
			return m, nil
		}
		v.working = false
		v.steering = true
		v.finishRunningCards()
		return m, nil

	case diffMsg:
		m.applyDiff(msg)
		if m.screen == screenReview {
			if m.rvSel > len(m.diff) {
				m.rvSel = len(m.diff)
			}
			m.renderReview()
		}
		return m, nil

	case stagedMsg:
		return m, m.applyStaged(msg)

	case draftMsg:
		return m, m.applyDraft(msg)

	case committedMsg:
		return m, m.applyCommitted(msg)

	case restoredMsg:
		return m, m.applyRestored(msg)

	case sessionsMsg:
		m.sessions = msg.list
		if m.cursor >= len(m.sessions) {
			m.cursor = 0
		}
		return m, nil

	case errMsg:
		v := m.viewOf(msg.id)
		if v == nil {
			v = m.sview
		}
		v.errText = msg.err.Error()
		v.working = false
		return m, nil

	case tickMsg:
		if m.anyWorking() {
			return m, tick()
		}
		m.ticking = false
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
	if m.screen == screenDiff {
		return m.keyDiff(msg)
	}
	if m.screen == screenReview {
		return m.keyReview(msg)
	}
	if m.busyAsk != "" {
		return m.keyFolderBusy(msg)
	}

	switch msg.String() {
	case "ctrl+c":
		m.quitAll()
		return m, tea.Quit
	case "ctrl+l":
		m.screen = screenList
		return m, m.loadSessions()
	case "ctrl+n":
		return m, m.startDraft()
	case "ctrl+o":
		m.expanded = !m.expanded
		m.unfreezeClipped()
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
	case "d":
		// Diff-so-far is one key from anywhere in the session, any
		// moment, including mid-run. An empty composer is the tell that
		// d is a command and not the start of a sentence.
		if m.ta.Value() == "" && !m.draft {
			return m.openDiff()
		}
	case "enter":
		text := strings.TrimSpace(m.ta.Value())
		if text == "" {
			return m, nil
		}
		if m.working {
			m.errText = "a turn is running; esc stops it first"
			return m, nil
		}
		if m.waiting {
			m.errText = "a message is already waiting for the folder"
			return m, nil
		}
		m.ta.Reset()
		m.layout()
		if m.reqDiff != "" {
			// Request-changes: the agent sees its own diff and the
			// complaint together, same session, same thread.
			text += "\n\nHere is your diff so far, the changes I am asking about:\n```diff\n" + m.reqDiff + "\n```"
			m.reqDiff, m.reqBanner = "", ""
		}
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

// keyFolderBusy answers the wait-or-cancel ask: y parks the message with
// the engine, anything that reads as no puts it back in the composer.
func (m *model) keyFolderBusy(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	text := m.busyAsk
	m.busyAsk, m.busyWith = "", ""
	switch msg.String() {
	case "ctrl+c":
		m.quitAll()
		return m, tea.Quit
	case "y", "Y", "enter":
		return m, m.queueSend(text)
	}
	m.ta.SetValue(text)
	m.layout()
	return m, nil
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
		return m, m.startDraft()
	case "enter":
		if len(m.sessions) == 0 {
			return m, nil
		}
		id := m.sessions[m.cursor].ID
		if v, ok := m.open[id]; ok {
			// Already open: focus the live view, no replay needed.
			return m, m.focus(v)
		}
		return m, m.openSession(id, "")
	}
	return m, nil
}

// viewOf finds the open view for a session; nil when it was never opened
// this run. The focused view answers for the draft's empty id.
func (m *model) viewOf(id waggle.SessionID) *sview {
	if m.sess.ID == id {
		return m.sview
	}
	return m.open[id]
}

// focus makes v the visible session. Whatever was focused keeps its
// watch and its state; it just stops being drawn.
func (m *model) focus(v *sview) tea.Cmd {
	if m.sview == v {
		m.screen = screenChat
		return nil
	}
	m.ta.Blur()
	m.sview = v
	m.screen = screenChat
	m.layout()
	m.refresh(true)
	return m.ta.Focus()
}

// startDraft opens a fresh conversation; running sessions keep running.
func (m *model) startDraft() tea.Cmd {
	if m.draft {
		m.screen = screenChat
		return nil
	}
	if m.scratch == nil {
		m.scratch = newSview()
	}
	return m.focus(m.scratch)
}

// quitAll cancels every open session's watch on the way out.
func (m *model) quitAll() {
	for _, v := range m.open {
		if v.cancel != nil {
			v.cancel()
		}
	}
}

func (m *model) anyWorking() bool {
	if m.working {
		return true
	}
	for _, v := range m.open {
		if v.working {
			return true
		}
	}
	return false
}

// verbs are what the hive is doing while a turn runs. One is picked per
// turn so the working line has some life without being noisy.
var verbs = []string{"Buzzing", "Foraging", "Waggling", "Humming", "Pollinating", "Combing", "Swarming", "Dancing"}

func pickVerb(t time.Time) string {
	return verbs[int(t.UnixNano()/1000)%len(verbs)]
}

// apply folds one event into a session's transcript and status. Only the
// focused view redraws; a background view just keeps its state current so
// switching to it is instant.
func (m *model) apply(v *sview, ev waggle.Event) {
	if ev.Seq != 0 && ev.Seq <= v.lastSeq {
		return
	}
	v.lastSeq = ev.Seq
	switch ev.Kind {
	case waggle.KindPulse:
		// A pulse carries the whole picture so far; replace, never add.
		if p, ok := decode[waggle.Pulse](ev.Data); ok {
			v.pulse = p
		}
		return
	case waggle.KindCost:
		if c, ok := decode[waggle.Cost](ev.Data); ok {
			if c.Live {
				// Older journals carried live snapshots as costs.
				v.pulse.Usage = c
				return
			}
			v.tokens.InputTokens += c.InputTokens
			v.tokens.CachedInputTokens += c.CachedInputTokens
			v.tokens.OutputTokens += c.OutputTokens
			v.tokens.ReasoningTokens += c.ReasoningTokens
			v.pulse.Usage = waggle.Cost{}
		}
		return
	case waggle.KindResult:
		v.working = false
		v.pulse.Usage = waggle.Cost{}
		v.finishRunningCards()
		m.touch(v, false)
		return
	case waggle.KindDied:
		v.working = false
		v.pulse.Usage = waggle.Cost{}
	case waggle.KindMessage:
		if ev.Bee == "human" {
			v.working = true
			v.waiting = false // a queued turn just started
			v.steering = false
			if v.started.IsZero() {
				v.started = time.Now()
			}
			if v.verb == "" {
				v.verb = pickVerb(v.started)
			}
		}
	case waggle.KindTool, waggle.KindEdit, waggle.KindPlan:
		if idx, ok := v.byRef[refKey(ev)]; ok {
			prev := v.items[idx]
			prev.ev = ev
			prev.frozen = false
			m.touch(v, false)
			return
		}
	}
	it := &item{ev: ev, start: ev.At}
	v.items = append(v.items, it)
	if k := refKey(ev); k != "" {
		v.byRef[k] = len(v.items) - 1
	}
	m.touch(v, true)
}

// touch redraws the transcript when the changed view is the visible one.
func (m *model) touch(v *sview, past bool) {
	if v != m.sview {
		return
	}
	follow := m.vp.AtBottom()
	if past {
		follow = follow || m.vp.PastBottom()
	}
	m.refresh(follow)
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
func (v *sview) finishRunningCards() {
	for _, it := range v.items {
		it.frozen = true
	}
}

// unfreezeClipped re-renders every card whose body clips: tool output and
// edit diffs both honor the expand toggle.
func (m *model) unfreezeClipped() {
	for _, it := range m.items {
		if it.ev.Kind == waggle.KindTool || it.ev.Kind == waggle.KindEdit {
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
	m.dvp.SetWidth(m.w)
	dh := m.h - 2 // header + hints
	if dh < 1 {
		dh = 1
	}
	m.dvp.SetHeight(dh)

	rw := m.w - m.rvTreeWidth()
	if rw < 1 {
		rw = 1
	}
	m.rvVP.SetWidth(rw)
	rh := m.h - 3 // header + footer + status
	if rh < 1 {
		rh = 1
	}
	m.rvVP.SetHeight(rh)
	m.rvTA.SetWidth(rw - 6)
	m.rvTA.SetHeight(min(10, max(3, rh-6)))
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
	case m.screen == screenDiff:
		content = m.viewDiff()
	case m.screen == screenReview:
		content = m.viewReview()
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
	case m.busyAsk != "":
		q := "Another session is already changing files in this folder; two at once would overwrite each other."
		if m.busyWith != "" {
			q = fmt.Sprintf("%q is already changing files in this folder; two at once would overwrite each other.", m.busyWith)
		}
		return " " + m.th.ToolBad.Render(truncate(q, m.w-24)) +
			m.th.Faint.Render("  ") + m.th.StatusKey.Render("y") + m.th.Faint.Render(" wait its turn · ") +
			m.th.StatusKey.Render("n") + m.th.Faint.Render(" cancel")
	case m.waiting:
		return " " + m.th.Faint.Render("◌ waiting for the folder · it starts when the other session is done")
	case m.reqBanner != "" && !m.working:
		return " " + m.th.StatusKey.Render(m.reqBanner) + m.th.Faint.Render(" · the diff rides along with your next message")
	case m.working:
		s := " " + m.spin.View() + " " + m.th.Brain.Render(m.verb+"…")
		parts := []string{time.Since(m.started).Round(time.Second).String()}
		if out := m.pulse.Usage.OutputTokens + m.pulse.Usage.ReasoningTokens; out > 0 {
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
	case m.waiting:
		left = " " + m.th.Faint.Render("◌") + m.th.StatusBar.Render(" waiting for the folder")
	default:
		left = " " + m.th.Faint.Render("○") + m.th.StatusBar.Render(" idle")
	}
	in := m.tokens.InputTokens + m.pulse.Usage.InputTokens
	out := m.tokens.OutputTokens + m.tokens.ReasoningTokens + m.pulse.Usage.OutputTokens + m.pulse.Usage.ReasoningTokens
	if in+out > 0 {
		left += m.th.StatusBar.Render(fmt.Sprintf("  ·  ↑ %s ↓ %s", compact(in), compact(out)))
		if cached := m.tokens.CachedInputTokens + m.pulse.Usage.CachedInputTokens; cached > 0 && in > 0 {
			left += m.th.StatusBar.Render(fmt.Sprintf(" · %d%% cached", cached*100/in))
		}
	}
	help := m.th.StatusKey.Render("enter") + m.th.Faint.Render(" send  ") +
		m.th.StatusKey.Render("esc") + m.th.Faint.Render(" stop  ")
	if !m.draft {
		help += m.th.StatusKey.Render("d") + m.th.Faint.Render(" diff  ")
	}
	help += m.th.StatusKey.Render("^o") + m.th.Faint.Render(" expand  ") +
		m.th.StatusKey.Render("^l") + m.th.Faint.Render(" sessions  ") +
		m.th.StatusKey.Render("^c") + m.th.Faint.Render(" quit ")
	right := help
	if v := m.viewVitals(); v != "" &&
		m.w-lipgloss.Width(left)-lipgloss.Width(v)-lipgloss.Width(help)-3 >= 1 {
		right = v + m.th.Faint.Render("  ·  ") + help
	}
	gap := m.w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = m.w - lipgloss.Width(left) - lipgloss.Width(help)
		if gap < 1 {
			return left
		}
		right = help
	}
	return left + strings.Repeat(" ", gap) + right
}

// viewVitals is the brain's health readout on the status bar: which model
// is thinking at what effort, how full its context window is, and how much
// of the plan's rate limits the hive has eaten. All of it comes from
// pulses, so a brain that reports nothing shows nothing.
func (m *model) viewVitals() string {
	var parts []string
	if m.pulse.Model != "" {
		s := m.pulse.Model
		if m.pulse.Effort != "" {
			s += " " + m.pulse.Effort
		}
		parts = append(parts, m.th.StatusBar.Render(s))
	}
	if m.pulse.Window > 0 && m.pulse.Context > 0 {
		pct := m.pulse.Context * 100 / m.pulse.Window
		parts = append(parts, meter(m.th, fmt.Sprintf("%d%% context", pct), float64(pct)))
	}
	for _, l := range m.pulse.Limits {
		parts = append(parts, meter(m.th, fmt.Sprintf("%s %.0f%%", l.Name, l.UsedPct), l.UsedPct))
	}
	return strings.Join(parts, m.th.Faint.Render(" · "))
}

// meter renders a usage figure, turning red once it is nearly spent.
func meter(t theme, s string, pct float64) string {
	if pct >= 80 {
		return t.ToolBad.Render(s)
	}
	return t.StatusBar.Render(s)
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
		detail := s.Updated.Format("Jan 2 15:04")
		if s.Branch != "" {
			// A worktree session names its branch here, so git branch
			// never shows the user one hachi did not mention.
			detail = s.Branch + " · " + detail
		}
		if s.State == hive.StateWaiting {
			detail = "waiting for the folder · " + detail
		}
		row := fmt.Sprintf("%s  %s  %s", stateDot(m.th, s.State), truncate(title, m.w-30),
			m.th.Faint.Render(detail))
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
	case hive.StateWaiting:
		return t.Faint.Render("◌")
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
