package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

// muxSvc plays the engine for multiplexing tests: it counts Watch calls
// so a test can prove that switching back to an open session never
// replays, and it records every Send.
type muxSvc struct {
	watches int
	opened  int
	waits   int
	sent    []string
}

func (f *muxSvc) Sessions(context.Context) ([]hive.SessionInfo, error) { return nil, nil }
func (f *muxSvc) Open(_ context.Context, id waggle.SessionID, dir, brain string) (hive.SessionInfo, error) {
	f.opened++
	if id == "" {
		id = waggle.SessionID(fmt.Sprintf("new-%d", f.opened))
	}
	return hive.SessionInfo{ID: id, Dir: dir, Brain: brain, InRepo: true}, nil
}
func (f *muxSvc) Send(_ context.Context, id waggle.SessionID, msg string) error {
	f.sent = append(f.sent, string(id)+": "+msg)
	return nil
}
func (f *muxSvc) Watch(context.Context, waggle.SessionID) (<-chan waggle.Event, error) {
	f.watches++
	ch := make(chan waggle.Event)
	close(ch) // waitEvent must never block a test pump
	return ch, nil
}
func (f *muxSvc) Stop(context.Context, waggle.SessionID) error { return nil }
func (f *muxSvc) Changes(context.Context, waggle.SessionID) ([]hive.FileDiff, error) {
	return nil, nil
}
func (f *muxSvc) Stage(context.Context, waggle.SessionID, []string) ([]string, error) {
	return nil, nil
}
func (f *muxSvc) CommitDraft(context.Context, waggle.SessionID) (string, error) { return "", nil }
func (f *muxSvc) Commit(context.Context, waggle.SessionID, string) (string, error) {
	return "", nil
}
func (f *muxSvc) Restore(context.Context, waggle.SessionID, []string) (hive.RestoreReport, error) {
	return hive.RestoreReport{}, nil
}
func (f *muxSvc) Queue(context.Context, waggle.SessionID, string) error { return nil }
func (f *muxSvc) Seen(context.Context, waggle.SessionID) error          { return nil }
func (f *muxSvc) KeepWaiting(context.Context, waggle.SessionID) error {
	f.waits++
	return nil
}
func (f *muxSvc) MergeBack(context.Context, waggle.SessionID) (hive.MergeReport, error) {
	return hive.MergeReport{}, nil
}

// openView injects an opened session the way openedMsg would, without
// running the returned commands (they include a blocking waitEvent).
func openView(t *testing.T, m *model, id waggle.SessionID) *sview {
	t.Helper()
	ch := make(chan waggle.Event, 8)
	m.Update(openedMsg{
		info:  hive.SessionInfo{ID: id, Title: string(id), Brain: "codex", InRepo: true},
		watch: ch,
		stop:  func() {},
	})
	v := m.open[id]
	if v == nil {
		t.Fatalf("openedMsg did not register view %s", id)
	}
	if m.sess.ID != id {
		t.Fatalf("openedMsg did not focus %s, focused %q", id, m.sess.ID)
	}
	return v
}

func msgEvent(seq uint64, bee, text string) waggle.Event {
	return waggle.Event{Seq: seq, Bee: bee, Kind: waggle.KindMessage, At: time.Now(),
		Data: waggle.Enc(waggle.Message{Text: text})}
}

// TestCtrlNLeavesSessionRunning is the S4 core: a new conversation opens
// while the old session keeps streaming into its background view.
func TestCtrlNLeavesSessionRunning(t *testing.T) {
	m := newModel(&muxSvc{}, Options{Dir: "/tmp", Brain: "codex"})
	m.w, m.h = 100, 40
	m.layout()

	v1 := openView(t, m, "s1")
	m.Update(sentMsg{id: "s1"})
	if !v1.working {
		t.Fatal("sentMsg must mark the session working")
	}

	key(t, m, "ctrl+n")
	if !m.draft || m.sess.ID != "" {
		t.Fatalf("ctrl+n must focus a fresh draft, got draft=%v id=%q", m.draft, m.sess.ID)
	}
	if len(m.items) != 0 {
		t.Fatalf("the draft transcript must start empty, has %d items", len(m.items))
	}

	// The old session streams on while the draft is up.
	_, cmd := m.Update(eventMsg{id: "s1", ev: msgEvent(1, "codex", "still going")})
	if cmd == nil {
		t.Fatal("a background event must re-arm its watch")
	}
	if len(v1.items) != 1 {
		t.Fatalf("background view must fold its events, has %d items", len(v1.items))
	}
	if !v1.working {
		t.Fatal("background view must stay working")
	}
	if len(m.items) != 0 {
		t.Fatal("background events must not leak into the focused draft")
	}
}

// TestSwitchBackNeedsNoReplay: returning to an open session reuses its
// live view, transcript, and half-typed composer; Watch is never called
// a second time.
func TestSwitchBackNeedsNoReplay(t *testing.T) {
	svc := &muxSvc{}
	m := newModel(svc, Options{Dir: "/tmp", Brain: "codex"})
	m.w, m.h = 100, 40
	m.layout()

	openView(t, m, "s1")
	m.Update(eventMsg{id: "s1", ev: msgEvent(1, "codex", "the answer")})
	m.ta.SetValue("half a thought")

	key(t, m, "ctrl+n")
	if m.ta.Value() != "" {
		t.Fatalf("the draft composer must start empty, has %q", m.ta.Value())
	}

	m.sessions = []hive.SessionInfo{{ID: "s1", Title: "s1"}}
	m.cursor = 0
	m.screen = screenList
	key(t, m, "enter")

	if m.sess.ID != "s1" {
		t.Fatalf("list enter must focus s1, focused %q", m.sess.ID)
	}
	if len(m.items) != 1 {
		t.Fatalf("transcript must survive the round trip, has %d items", len(m.items))
	}
	if m.ta.Value() != "half a thought" {
		t.Fatalf("composer text must survive the round trip, has %q", m.ta.Value())
	}
	if svc.watches != 0 {
		t.Fatalf("switching back must not call Watch, called %d times", svc.watches)
	}
}

// TestDraftSendOpensSecondSession: enter on a draft creates a session
// through the service while the first session stays open and working.
func TestDraftSendOpensSecondSession(t *testing.T) {
	svc := &muxSvc{}
	m := newModel(svc, Options{Dir: "/tmp", Brain: "codex"})
	m.w, m.h = 100, 40
	m.layout()

	v1 := openView(t, m, "s1")
	m.Update(sentMsg{id: "s1"})
	key(t, m, "ctrl+n")

	m.ta.SetValue("second task")
	key(t, m, "enter")

	if m.sess.ID != "new-1" {
		t.Fatalf("draft send must focus the created session, focused %q", m.sess.ID)
	}
	if len(svc.sent) != 1 || svc.sent[0] != "new-1: second task" {
		t.Fatalf("the message must reach the new session, sent %v", svc.sent)
	}
	if m.open["s1"] != v1 || !v1.working {
		t.Fatal("the first session must stay open and working")
	}
	if m.scratch != nil && m.scratch == m.sview {
		t.Fatal("a spent draft must not stay focused")
	}
}

func TestFolderBusyAskWaitAndCancel(t *testing.T) {
	svc := &fakeSvc{}
	m := newModel(svc, Options{Dir: "/tmp/plain", Brain: "codex"})
	m.w, m.h = 100, 30
	m.sess = hive.SessionInfo{ID: "s1", Title: "second task"}
	m.open["s1"] = m.sview
	m.layout()

	// The refusal turns into the wait-or-cancel ask above the composer.
	_, _ = m.Update(folderBusyMsg{id: "s1", text: "tidy the notes", with: "make a mess"})
	out := m.viewActivity()
	if !strings.Contains(out, `"make a mess"`) || !strings.Contains(out, "wait its turn") {
		t.Fatalf("the ask must name the other session and offer the wait:\n%s", out)
	}

	// n cancels: the message goes back in the composer, nothing queued.
	key(t, m, "n")
	if m.busyAsk != "" || m.ta.Value() != "tidy the notes" {
		t.Fatalf("cancel must restore the composer, ask=%q composer=%q", m.busyAsk, m.ta.Value())
	}
	if len(svc.queued) != 0 {
		t.Fatalf("cancel must not queue, got %v", svc.queued)
	}

	// Refused again, y parks it with the engine and the card says so.
	m.ta.Reset()
	_, _ = m.Update(folderBusyMsg{id: "s1", text: "tidy the notes", with: "make a mess"})
	key(t, m, "y")
	if len(svc.queued) != 1 || svc.queued[0] != "tidy the notes" {
		t.Fatalf("y must queue the refused message, got %v", svc.queued)
	}
	if !m.waiting {
		t.Fatal("a queued message must show the waiting state")
	}
	if s := m.viewStatus(); !strings.Contains(s, "waiting for the folder") {
		t.Errorf("the status line must say waiting, got %q", s)
	}
	if a := m.viewActivity(); !strings.Contains(a, "waiting for the folder") {
		t.Errorf("the activity line must say waiting, got %q", a)
	}

	// A second message cannot pile on while one waits.
	m.ta.SetValue("another thing")
	key(t, m, "enter")
	if len(svc.queued) != 1 {
		t.Fatalf("no second queue entry while one waits, got %v", svc.queued)
	}
	if m.errText == "" {
		t.Fatal("the refusal to pile on must say why")
	}

	// The queued turn starting is a plain human message on the watch;
	// waiting hands over to working right there.
	m.apply(m.sview, waggle.Event{Sess: "s1", Bee: "human", Kind: waggle.KindMessage,
		Data: waggle.Enc(waggle.Message{Text: "tidy the notes"})})
	if m.waiting {
		t.Fatal("the human message arriving means the queued turn started")
	}
}
