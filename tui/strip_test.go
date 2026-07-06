package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

func sinfo(id string, st hive.State, ready bool) hive.SessionInfo {
	return hive.SessionInfo{ID: waggle.SessionID(id), Title: id, State: st, DiffReady: ready}
}

// One quiet session must look like a plain chat: no strip, no row lost.
func TestStripHiddenWhenAlone(t *testing.T) {
	m := newModel(&muxSvc{}, Options{})
	m.w, m.h = 100, 40
	m.layout()
	before := m.vp.Height()

	m.sess.ID = "a"
	m.Update(sessionsMsg{list: []hive.SessionInfo{sinfo("a", hive.StateIdle, false)}})
	if m.stripOn {
		t.Fatal("one idle session must not raise the strip")
	}
	if m.vp.Height() != before {
		t.Fatal("hidden strip must not eat a row")
	}
	if strings.Contains(m.View().Content, "need you") {
		t.Fatal("no strip content should render")
	}
}

func TestStripCountsEverySegment(t *testing.T) {
	m := newModel(&muxSvc{}, Options{})
	m.w, m.h = 160, 40
	m.layout()
	before := m.vp.Height()

	// The focused session is excluded: it is on screen and speaks for
	// itself. "a" working must not show up in the counts.
	m.sess.ID = "a"
	needs := sinfo("b", hive.StateNeeds, false)
	needs.Reason, needs.Detail = "question", "should I also update the v1 endpoints?"
	m.Update(sessionsMsg{list: []hive.SessionInfo{
		sinfo("a", hive.StateWorking, false),
		needs,
		sinfo("c", hive.StateDied, false),
		sinfo("d", hive.StateIdle, true),
		sinfo("e", hive.StateWaiting, false),
		sinfo("f", hive.StateWorking, false),
	}})
	if !m.stripOn {
		t.Fatal("sessions needing the human must raise the strip")
	}
	if m.vp.Height() != before-1 {
		t.Fatalf("visible strip must take exactly one row, %d -> %d", before, m.vp.Height())
	}
	s := m.viewStrip()
	for _, want := range []string{"● 2 need you", "▸ 1 working", "◐ 1 diff ready", "◌ 1 waiting", "ctrl+l", "ctrl+n",
		"(b: should I also update the v1 endpoints?)"} {
		if !strings.Contains(s, want) {
			t.Errorf("strip missing %q in %q", want, s)
		}
	}
	if !strings.Contains(m.View().Content, "need you") {
		t.Fatal("the strip must render on the chat screen")
	}
}

// A question outranks a death for the named reason, however old the
// death is, and the reason never renders on a narrow terminal.
func TestStripTopReasonPriorityAndWidth(t *testing.T) {
	m := newModel(&muxSvc{}, Options{})
	m.w, m.h = 160, 40
	m.sess.ID = "a"
	died := sinfo("old", hive.StateDied, false)
	died.Reason, died.Detail = "died", "exit status 1"
	ask := sinfo("deps bump", hive.StateNeeds, false)
	ask.Reason, ask.Detail = "question", "run npm install?"
	ask.Raised = died.Raised.Add(time.Hour)
	m.Update(sessionsMsg{list: []hive.SessionInfo{sinfo("a", hive.StateIdle, false), died, ask}})
	if s := m.viewStrip(); !strings.Contains(s, "(deps bump: run npm install?)") {
		t.Errorf("question must be the named reason, got %q", s)
	}
	m.w = 100
	if s := m.viewStrip(); strings.Contains(s, "npm install") {
		t.Errorf("the named reason needs a wide terminal, got %q", s)
	}
}

// A raise on the focused session must not pull the strip up: the human
// is already looking at it.
func TestStripIgnoresFocusedSession(t *testing.T) {
	m := newModel(&muxSvc{}, Options{})
	m.w, m.h = 100, 40
	m.sess.ID = "a"
	mine := sinfo("a", hive.StateNeeds, true)
	mine.Reason = "question"
	m.Update(sessionsMsg{list: []hive.SessionInfo{mine}})
	if m.stripOn {
		t.Fatal("the focused session's own raise must not show in the strip")
	}
}

func TestStripSingularAndNoHintsWhenNarrow(t *testing.T) {
	m := newModel(&muxSvc{}, Options{})
	m.w, m.h = 80, 40
	m.sess.ID = "a"
	m.Update(sessionsMsg{list: []hive.SessionInfo{
		sinfo("a", hive.StateIdle, false),
		sinfo("b", hive.StateNeeds, false),
	}})
	s := m.viewStrip()
	if !strings.Contains(s, "● 1 needs you") {
		t.Errorf("one session reads singular, got %q", s)
	}
	if strings.Contains(s, "ctrl+l") {
		t.Errorf("hints need a wide terminal, got %q", s)
	}
}

// Segments drop from the right when the terminal is narrow; the
// need-you count is the last thing standing.
func TestStripDropsRightFirst(t *testing.T) {
	m := newModel(&muxSvc{}, Options{})
	m.w, m.h = 20, 40
	m.sess.ID = "a"
	m.Update(sessionsMsg{list: []hive.SessionInfo{
		sinfo("a", hive.StateIdle, false),
		sinfo("b", hive.StateNeeds, false),
		sinfo("c", hive.StateNeeds, false),
		sinfo("d", hive.StateWorking, false),
		sinfo("e", hive.StateWaiting, false),
	}})
	s := m.viewStrip()
	if !strings.Contains(s, "need you") {
		t.Errorf("need-you survives any width, got %q", s)
	}
	if strings.Contains(s, "working") || strings.Contains(s, "waiting") {
		t.Errorf("narrow strip keeps only the leftmost segment, got %q", s)
	}
}

// Open-but-quiet sessions still earn the strip, saying only that much.
func TestStripShowsOpenCountWhenAllQuiet(t *testing.T) {
	m := newModel(&muxSvc{}, Options{})
	m.w, m.h = 100, 40
	m.sess.ID = "a"
	openView(t, m, "a")
	openView(t, m, "b")
	m.Update(sessionsMsg{list: []hive.SessionInfo{
		sinfo("a", hive.StateIdle, false),
		sinfo("b", hive.StateIdle, false),
	}})
	if !m.stripOn {
		t.Fatal("two open sessions must raise the strip")
	}
	if s := m.viewStrip(); !strings.Contains(s, "2 open") {
		t.Errorf("quiet strip says how many are open, got %q", s)
	}
}
