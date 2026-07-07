package tui

import (
	"context"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

// boardSvc serves a fixed session list so openBoard's refresh does not
// wipe what a test staged.
type boardSvc struct {
	muxSvc
	list []hive.SessionInfo
}

func (b *boardSvc) Sessions(context.Context) ([]hive.SessionInfo, error) { return b.list, nil }

func boardModel(t *testing.T, list []hive.SessionInfo) (*model, *boardSvc) {
	t.Helper()
	svc := &boardSvc{list: list}
	m := newModel(svc, Options{Dir: "/tmp", Brain: "codex"})
	m.w, m.h = 120, 40
	m.layout()
	m.Update(sessionsMsg{list: list})
	return m, svc
}

// Every state lands in exactly one column, and the orderings hold: the
// needs column triages by reason then oldest raise, the rest read
// freshest first.
func TestBoardColumns(t *testing.T) {
	base := time.Now()
	ask := sinfo("ask", hive.StateNeeds, false)
	ask.Reason, ask.Raised = "question", base.Add(time.Hour)
	dead := sinfo("dead", hive.StateDied, false)
	dead.Reason, dead.Raised = "died", base
	oldDiff := sinfo("old-diff", hive.StateNeeds, true)
	oldDiff.Reason, oldDiff.Raised = "diff", base
	newDiff := sinfo("new-diff", hive.StateNeeds, true)
	newDiff.Reason, newDiff.Raised = "diff", base.Add(time.Minute)

	work := sinfo("work", hive.StateWorking, false)
	work.Updated = base
	queued := sinfo("queued", hive.StateWaiting, false)
	queued.Updated = base.Add(time.Minute)

	parked := sinfo("parked", hive.StateIdle, true)
	settled := sinfo("settled", hive.StateIdle, false)
	settled.Updated = base
	fresh := sinfo("fresh", hive.StateIdle, false)
	fresh.Updated = base.Add(time.Hour)

	m, _ := boardModel(t, []hive.SessionInfo{
		newDiff, settled, work, oldDiff, parked, dead, fresh, queued, ask,
	})
	cols := m.boardColumns()

	names := func(c int) []string {
		var out []string
		for _, s := range cols[c] {
			out = append(out, s.Title)
		}
		return out
	}
	want := map[int][]string{
		colNeeds:   {"ask", "dead", "old-diff", "new-diff"},
		colWorking: {"queued", "work"},
		colReview:  {"parked"},
		colDone:    {"fresh", "settled"},
	}
	for c, w := range want {
		got := names(c)
		if strings.Join(got, ",") != strings.Join(w, ",") {
			t.Errorf("%s column: got %v want %v", colNames[c], got, w)
		}
	}
}

// Tab flips to the board from the chat, even mid-sentence, and flips
// back; the list screen has the same key.
func TestBoardTabToggles(t *testing.T) {
	m, _ := boardModel(t, []hive.SessionInfo{sinfo("a", hive.StateIdle, false)})
	m.sess.ID = "a"
	m.ta.SetValue("half a thou")
	key(t, m, "tab")
	if m.screen != screenBoard {
		t.Fatalf("tab from chat must open the board, on screen %d", m.screen)
	}
	if !strings.Contains(m.View().Content, "needs you 0") {
		t.Fatal("the board must render its column headers")
	}
	key(t, m, "tab")
	if m.screen != screenChat {
		t.Fatal("tab on the board must go back to the chat")
	}
	if m.ta.Value() != "half a thou" {
		t.Fatalf("the composer must survive the round trip, has %q", m.ta.Value())
	}

	m.screen = screenList
	key(t, m, "tab")
	if m.screen != screenBoard {
		t.Fatal("tab from the list must open the board")
	}
	for _, back := range []string{"q", "esc"} {
		m.screen = screenBoard
		key(t, m, back)
		if m.screen != screenChat {
			t.Fatalf("%s must leave the board", back)
		}
	}
}

// The cursor lands on the top of needs-you when anything is raised;
// with nothing raised it keeps its place, clamped to what exists.
func TestBoardCursorStartsOnNeeds(t *testing.T) {
	ask := sinfo("ask", hive.StateNeeds, false)
	ask.Reason = "question"
	m, svc := boardModel(t, []hive.SessionInfo{sinfo("w", hive.StateWorking, false), ask})
	m.bdCol, m.bdRow = colDone, 3
	key(t, m, "tab")
	if m.bdCol != colNeeds || m.bdRow != 0 {
		t.Fatalf("the cursor must jump to needs-you, at col %d row %d", m.bdCol, m.bdRow)
	}

	svc.list = []hive.SessionInfo{sinfo("w", hive.StateWorking, false)}
	m.Update(sessionsMsg{list: svc.list})
	m.screen, m.bdCol, m.bdRow = screenChat, colWorking, 5
	key(t, m, "tab")
	if m.bdCol != colWorking || m.bdRow != 0 {
		t.Fatalf("with nothing raised the cursor stays put, clamped: col %d row %d", m.bdCol, m.bdRow)
	}
}

// h/l walk the four columns and stop at the edges; j/k walk the cards
// and stop likewise; changing column re-clamps the row.
func TestBoardCursorClamps(t *testing.T) {
	a := sinfo("a", hive.StateIdle, false)
	b := sinfo("b", hive.StateIdle, false)
	b.Updated = a.Updated.Add(time.Minute)
	m, _ := boardModel(t, []hive.SessionInfo{a, b})
	m.screen = screenBoard
	m.bdCol, m.bdRow = colWorking, 0

	key(t, m, "h")
	if m.bdCol != colWorking {
		t.Fatalf("h at the left edge must stay, col %d", m.bdCol)
	}
	for range 5 {
		key(t, m, "l")
	}
	if m.bdCol != colDone {
		t.Fatalf("l must stop at done, col %d", m.bdCol)
	}
	key(t, m, "j")
	key(t, m, "j")
	key(t, m, "j")
	if m.bdRow != 1 {
		t.Fatalf("j must stop at the last card, row %d", m.bdRow)
	}
	key(t, m, "h")
	if m.bdCol != colReview || m.bdRow != 0 {
		t.Fatalf("moving into an empty column must clamp the row, col %d row %d", m.bdCol, m.bdRow)
	}
	key(t, m, "k")
	if m.bdRow != 0 {
		t.Fatalf("k at the top must stay, row %d", m.bdRow)
	}
}

// Enter on a card focuses the session: an open one by pointer swap with
// no second Watch, an unopened one through the service.
func TestBoardEnterOpens(t *testing.T) {
	m, svc := boardModel(t, nil)
	openView(t, m, "s1")
	openView(t, m, "s2")
	svc.list = []hive.SessionInfo{sinfo("s1", hive.StateIdle, false), sinfo("cold", hive.StateIdle, false)}
	m.Update(sessionsMsg{list: svc.list})

	m.screen, m.bdCol, m.bdRow = screenBoard, colDone, 0
	for i, s := range m.boardColumns()[colDone] {
		if s.ID == "s1" {
			m.bdRow = i
		}
	}
	key(t, m, "enter")
	if m.sess.ID != "s1" {
		t.Fatalf("enter must focus the card's session, focused %q", m.sess.ID)
	}
	if m.screen != screenChat {
		t.Fatal("focusing must land back in the chat")
	}
	if svc.watches != 0 {
		t.Fatalf("an open session must not be watched again, Watch called %d times", svc.watches)
	}

	m.screen, m.bdCol, m.bdRow = screenBoard, colDone, 0
	for i, s := range m.boardColumns()[colDone] {
		if s.ID == "cold" {
			m.bdRow = i
		}
	}
	opened := svc.opened
	key(t, m, "enter")
	if svc.opened != opened+1 {
		t.Fatalf("an unopened session must go through the service, Open called %d times", svc.opened)
	}
	if m.sess.ID != "cold" {
		t.Fatalf("enter must focus the cold session, focused %q", m.sess.ID)
	}
}

// Below 100 columns the board goes one column at a time: the header row
// brackets the selected column and only its cards render.
func TestBoardNarrow(t *testing.T) {
	ask := sinfo("ask me", hive.StateNeeds, false)
	ask.Reason, ask.Detail = "question", "which one?"
	m, _ := boardModel(t, []hive.SessionInfo{ask, sinfo("busy", hive.StateWorking, false)})
	m.w = 80
	m.screen, m.bdCol, m.bdRow = screenBoard, colNeeds, 0

	out := m.View().Content
	if !strings.Contains(out, "[needs you 1]") {
		t.Fatalf("the selected header must be bracketed:\n%s", out)
	}
	if !strings.Contains(out, "ask me") {
		t.Fatal("the selected column's cards must render")
	}
	if strings.Contains(out, "busy") {
		t.Fatal("other columns' cards must not render narrow")
	}
	key(t, m, "h")
	if !strings.Contains(m.View().Content, "busy") {
		t.Fatal("h must page to the working column")
	}
}

// A column taller than the screen says how much fell off.
func TestBoardOverflow(t *testing.T) {
	var list []hive.SessionInfo
	for _, n := range []string{"one", "two", "three", "four", "five", "six", "seven", "eight"} {
		list = append(list, sinfo(n, hive.StateIdle, false))
	}
	m, _ := boardModel(t, list)
	m.h = 18 // room for two cards a column
	m.screen, m.bdCol, m.bdRow = screenBoard, colDone, 0
	if out := m.View().Content; !strings.Contains(out, "+6 more") {
		t.Fatalf("the overflow line must count what fell off:\n%s", out)
	}
	// Walking past the window scrolls it: the selected card is always
	// on screen.
	for range 7 {
		key(t, m, "j")
	}
	if out := m.View().Content; !strings.Contains(out, list[7].Title) {
		t.Fatal("the cursor's card must scroll into view")
	}
}

// The strip and the board are two renderings of one derivation: for any
// session set their numbers agree.
func TestBoardStripAgreement(t *testing.T) {
	states := []hive.State{hive.StateIdle, hive.StateWorking, hive.StateWaiting, hive.StateNeeds, hive.StateDied}
	reasons := []string{"", "question", "died", "stall", "diff"}
	rng := rand.New(rand.NewSource(2086))
	for round := 0; round < 200; round++ {
		var list []hive.SessionInfo
		for i := 0; i < rng.Intn(12); i++ {
			s := sinfo(string(rune('a'+i)), states[rng.Intn(len(states))], rng.Intn(2) == 0)
			s.Reason = reasons[rng.Intn(len(reasons))]
			list = append(list, s)
		}
		m, _ := boardModel(t, list)
		m.sess.ID = "" // the strip skips the focused session; compare whole sets
		c := m.stripCounts()
		cols := m.boardColumns()
		if c.needs != len(cols[colNeeds]) {
			t.Fatalf("round %d: strip needs %d, board %d", round, c.needs, len(cols[colNeeds]))
		}
		if c.working+c.waiting != len(cols[colWorking]) {
			t.Fatalf("round %d: strip working %d+%d, board %d", round, c.working, c.waiting, len(cols[colWorking]))
		}
		if c.ready != len(cols[colReview]) {
			t.Fatalf("round %d: strip ready %d, board %d", round, c.ready, len(cols[colReview]))
		}
		total := len(cols[0]) + len(cols[1]) + len(cols[2]) + len(cols[3])
		if total != len(list) {
			t.Fatalf("round %d: %d sessions, %d cards", round, len(list), total)
		}
	}
}

// The card face: state glyph and title on top, brain and age in the
// middle, the last action in plain words below, worded per the spec.
func TestBoardCardFace(t *testing.T) {
	m, svc := boardModel(t, nil)
	v := openView(t, m, "s1")
	exit := 1
	v.items = append(v.items,
		&item{ev: waggle.Event{Kind: waggle.KindMessage, Bee: "codex",
			Data: waggle.Enc(waggle.Message{Text: "done with the rename"})}},
		&item{ev: waggle.Event{Kind: waggle.KindTool,
			Data: waggle.Enc(waggle.Tool{Command: "go test", Status: "completed", ExitCode: &exit})}},
	)
	info := sinfo("s1", hive.StateWorking, false)
	info.Brain = "codex"
	info.Updated = time.Now().Add(-3 * time.Minute)
	svc.list = []hive.SessionInfo{info}
	m.Update(sessionsMsg{list: svc.list})
	m.screen, m.bdCol, m.bdRow = screenBoard, colWorking, 0

	out := m.View().Content
	for _, want := range []string{"s1", "codex", "3m", "ran go test (exit 1)"} {
		if !strings.Contains(out, want) {
			t.Errorf("card missing %q:\n%s", want, out)
		}
	}

	// A raise puts its reason on the card, ahead of any event.
	info.State, info.Reason, info.Detail = hive.StateNeeds, "question", "should I push?"
	svc.list = []hive.SessionInfo{info}
	m.Update(sessionsMsg{list: svc.list})
	m.bdCol = colNeeds
	if out := m.View().Content; !strings.Contains(out, "should I push?") {
		t.Errorf("a raised card must say why:\n%s", out)
	}

	// Never-opened sessions still get a sensible third row.
	cold := sinfo("cold", hive.StateIdle, true)
	svc.list = []hive.SessionInfo{cold}
	m.Update(sessionsMsg{list: svc.list})
	m.bdCol = colReview
	if out := m.View().Content; !strings.Contains(out, "changes to review") {
		t.Errorf("a cold card falls back to state words:\n%s", out)
	}
}
