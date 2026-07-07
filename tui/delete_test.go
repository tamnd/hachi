package tui

// x delete: one plain sentence, y or nothing. The confirm lives on both
// the list and the board, a working session's ask says the run stops
// first, and deleting the focused session drops its view without
// touching any other open session.

import (
	"strings"
	"testing"

	"github.com/tamnd/hachi/hive"
)

func TestListDeleteAsksInOneSentence(t *testing.T) {
	m, svc := boardModel(t, []hive.SessionInfo{
		sinfo("keep me", hive.StateIdle, false),
		sinfo("drop me", hive.StateWorking, false),
	})
	m.screen = screenList

	m.cursor = 1
	key(t, m, "x")
	if m.delAsk == "" {
		t.Fatal("x must raise the delete ask")
	}
	out := m.View().Content
	if !strings.Contains(out, "delete \"drop me\"?") {
		t.Fatalf("the ask must name the session:\n%s", out)
	}
	if !strings.Contains(out, "the run stops first") {
		t.Fatalf("deleting a working session must say the run stops first:\n%s", out)
	}

	// Anything but y is a no.
	key(t, m, "n")
	if m.delAsk != "" || len(svc.deleted) != 0 {
		t.Fatalf("n must cancel, deleted %v", svc.deleted)
	}

	key(t, m, "x")
	key(t, m, "y")
	if len(svc.deleted) != 1 || svc.deleted[0] != "drop me" {
		t.Fatalf("y must delete the session under the cursor, deleted %v", svc.deleted)
	}
	if !strings.Contains(m.View().Content, "deleted") {
		t.Fatal("the outcome must show under the list")
	}
}

func TestDeleteWordsWhatSurvives(t *testing.T) {
	m, svc := boardModel(t, []hive.SessionInfo{sinfo("branchy", hive.StateIdle, false)})
	svc.deleteKept = "its commits stay on hachi/branchy"
	m.screen = screenList
	key(t, m, "x")
	key(t, m, "y")
	if !strings.Contains(m.View().Content, "deleted; its commits stay on hachi/branchy") {
		t.Fatalf("the note must carry the engine's sentence:\n%s", m.View().Content)
	}
}

func TestBoardDeleteConfirm(t *testing.T) {
	m, svc := boardModel(t, []hive.SessionInfo{sinfo("card", hive.StateIdle, false)})
	m.screen, m.bdCol, m.bdRow = screenBoard, colDone, 0

	key(t, m, "x")
	out := m.View().Content
	if !strings.Contains(out, "delete \"card\" and its whole history?") {
		t.Fatalf("the board footer must carry the ask:\n%s", out)
	}
	key(t, m, "esc")
	if len(svc.deleted) != 0 {
		t.Fatal("esc must cancel the ask")
	}
	key(t, m, "x")
	key(t, m, "y")
	if len(svc.deleted) != 1 || svc.deleted[0] != "card" {
		t.Fatalf("y must delete the selected card, deleted %v", svc.deleted)
	}
}

func TestDeleteFocusedSessionDropsItsView(t *testing.T) {
	m, svc := boardModel(t, nil)
	other := openView(t, m, "other")
	openView(t, m, "gone")
	svc.list = []hive.SessionInfo{sinfo("other", hive.StateIdle, false), sinfo("gone", hive.StateIdle, false)}
	m.Update(sessionsMsg{list: svc.list})
	if m.sess.ID != "gone" {
		t.Fatalf("setup: the doomed session must be focused, got %q", m.sess.ID)
	}

	m.screen = screenList
	for i, s := range m.sessions {
		if s.ID == "gone" {
			m.cursor = i
		}
	}
	key(t, m, "x")
	key(t, m, "y")
	if _, ok := m.open["gone"]; ok {
		t.Fatal("the deleted session's view must be dropped")
	}
	if m.sess.ID == "gone" {
		t.Fatal("focus must leave the deleted session")
	}
	if !m.draft {
		t.Fatal("focus falls back to a fresh draft")
	}
	if _, ok := m.open["other"]; !ok || other.cancel == nil {
		t.Fatal("other open sessions must keep their views")
	}
}

// The committed wording: a worktree session whose work is committed says
// so on its list row and its board card, the cue that merge-back is next.
func TestCommittedWording(t *testing.T) {
	s := sinfo("port lexer", hive.StateIdle, false)
	s.Branch, s.Committed = "hachi/port-lexer-v2", true
	m, _ := boardModel(t, []hive.SessionInfo{s})

	m.screen = screenList
	if out := m.View().Content; !strings.Contains(out, "committed on hachi/port-lexer-v2") {
		t.Fatalf("the list row must say committed on the branch:\n%s", out)
	}

	m.w = 180 // wide enough that the card row fits untruncated
	m.layout()
	m.screen, m.bdCol, m.bdRow = screenBoard, colDone, 0
	if out := m.View().Content; !strings.Contains(out, "committed on hachi/port-lexer-v2") {
		t.Fatalf("the board card must say committed on the branch:\n%s", out)
	}

	// Uncommitted work outranks the mark: a diff waiting for review is
	// the fresher story.
	s.DiffReady = true
	m2, _ := boardModel(t, []hive.SessionInfo{s})
	m2.screen, m2.bdCol, m2.bdRow = screenBoard, colReview, 0
	if out := m2.View().Content; !strings.Contains(out, "changes to review") {
		t.Fatalf("a ready diff outranks the committed mark:\n%s", out)
	}
}
