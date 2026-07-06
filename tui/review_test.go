package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbles/v2/cursor"
	tea "charm.land/bubbletea/v2"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

// fakeSvc plays the engine for review tests: it serves a mutable change
// set and records which verbs the screen actually asked for.
type fakeSvc struct {
	diffs    []hive.FileDiff
	staged   [][]string
	restored [][]string
	commits  []string
	draft    string
	draftErr error
	nonGit   bool
	branch   string           // worktree branch the fake session reports
	merges   int              // MergeBack calls
	mergeRep hive.MergeReport // what MergeBack answers
	queued   []string         // Queue calls
}

func (f *fakeSvc) Sessions(context.Context) ([]hive.SessionInfo, error) { return nil, nil }
func (f *fakeSvc) Open(_ context.Context, id waggle.SessionID, dir, brain string) (hive.SessionInfo, error) {
	return hive.SessionInfo{ID: "s1", Title: "make a mess", Dir: dir, Brain: brain, InRepo: !f.nonGit, Branch: f.branch}, nil
}
func (f *fakeSvc) Send(context.Context, waggle.SessionID, string) error { return nil }
func (f *fakeSvc) Queue(_ context.Context, _ waggle.SessionID, msg string) error {
	f.queued = append(f.queued, msg)
	return nil
}
func (f *fakeSvc) MergeBack(context.Context, waggle.SessionID) (hive.MergeReport, error) {
	f.merges++
	return f.mergeRep, nil
}
func (f *fakeSvc) Watch(context.Context, waggle.SessionID) (<-chan waggle.Event, error) {
	return nil, nil
}
func (f *fakeSvc) Stop(context.Context, waggle.SessionID) error        { return nil }
func (f *fakeSvc) Seen(context.Context, waggle.SessionID) error        { return nil }
func (f *fakeSvc) KeepWaiting(context.Context, waggle.SessionID) error { return nil }
func (f *fakeSvc) Changes(context.Context, waggle.SessionID) ([]hive.FileDiff, error) {
	return f.diffs, nil
}
func (f *fakeSvc) Stage(_ context.Context, _ waggle.SessionID, paths []string) ([]string, error) {
	f.staged = append(f.staged, paths)
	var out []string
	for i := range f.diffs {
		if paths == nil && f.diffs[i].Outside {
			continue
		}
		if paths != nil && !contains(paths, f.diffs[i].Path) {
			continue
		}
		f.diffs[i].Staged = true
		out = append(out, f.diffs[i].Path)
	}
	return out, nil
}
func (f *fakeSvc) CommitDraft(context.Context, waggle.SessionID) (string, error) {
	return f.draft, f.draftErr
}
func (f *fakeSvc) Commit(_ context.Context, _ waggle.SessionID, message string) (string, error) {
	f.commits = append(f.commits, message)
	return "[main abc1234] " + strings.SplitN(message, "\n", 2)[0], nil
}
func (f *fakeSvc) Restore(_ context.Context, _ waggle.SessionID, paths []string) (hive.RestoreReport, error) {
	f.restored = append(f.restored, paths)
	var rep hive.RestoreReport
	var left []hive.FileDiff
	for _, d := range f.diffs {
		switch {
		case paths == nil && d.Outside:
			rep.Skipped = append(rep.Skipped, hive.RestoreSkip{Path: d.Path, Reason: "you changed it yourself"})
			left = append(left, d)
		case paths == nil || contains(paths, d.Path):
			rep.Restored = append(rep.Restored, d.Path)
		default:
			left = append(left, d)
		}
	}
	f.diffs = left
	return rep, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// pump runs a command chain to quiescence, feeding every message back
// through Update the way the runtime would.
func pump(t *testing.T, m *model, cmd tea.Cmd) {
	t.Helper()
	for cmd != nil {
		msg := cmd()
		if msg == nil {
			return
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				pump(t, m, c)
			}
			return
		}
		// A cursor blink fed back through Update arms the next blink, and
		// the chain never quiesces. The runtime lives with that; a test
		// pump must drop it.
		if _, ok := msg.(cursor.BlinkMsg); ok {
			return
		}
		_, cmd = m.Update(msg)
	}
}

func key(t *testing.T, m *model, k string) {
	t.Helper()
	var msg tea.KeyPressMsg
	switch k {
	case "enter":
		msg = tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		msg = tea.KeyPressMsg{Code: tea.KeyEscape}
	case "ctrl+d":
		msg = tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}
	default:
		msg = tea.KeyPressMsg{Code: rune(k[0]), Text: k}
	}
	_, cmd := m.Update(msg)
	pump(t, m, cmd)
}

func reviewGallery() []hive.FileDiff {
	return []hive.FileDiff{
		{Path: "hello.sh", Status: "A", Patch: "@@ -0,0 +1,2 @@\n+#!/bin/sh\n+echo hi"},
		{Path: "mine.txt", Status: "M", Outside: true, Patch: "@@ -1 +1 @@\n-agent\n+mine"},
		{Path: "notes.txt", Status: "M", Patch: "@@ -1,2 +1,3 @@\n alpha\n beta\n+GAMMA"},
	}
}

func newReviewModel(t *testing.T, svc *fakeSvc) *model {
	t.Helper()
	m := newModel(svc, Options{Dir: "/tmp", Brain: "codex"})
	m.w, m.h = 110, 32
	m.draft = false
	m.sess = hive.SessionInfo{ID: "s1", Title: "make a mess", InRepo: !svc.nonGit}
	m.layout()
	_, cmd := m.openReview()
	pump(t, m, cmd)
	return m
}

func TestReviewScreenRenders(t *testing.T) {
	m := newReviewModel(t, &fakeSvc{diffs: reviewGallery()})
	out := m.viewReview()
	for _, want := range []string{"review", "make a mess", "3 files changed", "all changes",
		"hello.sh", "notes.txt", "!", "○", "stage+commit", "undo all", "request changes"} {
		if !strings.Contains(out, want) {
			t.Errorf("review screen misses %q:\n%s", want, out)
		}
	}
}

func TestReviewStageAll(t *testing.T) {
	svc := &fakeSvc{diffs: reviewGallery()}
	m := newReviewModel(t, svc)

	key(t, m, "a") // summary row: blanket stage
	if len(svc.staged) != 1 || svc.staged[0] != nil {
		t.Fatalf("blanket a must call Stage(nil), got %+v", svc.staged)
	}
	if !strings.Contains(m.rvStatus, "2 files staged") {
		t.Errorf("status should count what was staged (outside skipped), got %q", m.rvStatus)
	}
	out := m.viewReview()
	if !strings.Contains(out, "●") {
		t.Errorf("staged rows need their marker:\n%s", out)
	}

	// The flagged file needs an individual a, and naming it works.
	key(t, m, "j")
	key(t, m, "j") // mine.txt
	key(t, m, "a")
	if got := svc.staged[len(svc.staged)-1]; len(got) != 1 || got[0] != "mine.txt" {
		t.Errorf("file-scoped a must name the path, got %v", got)
	}
}

func TestReviewCommitDraftFlow(t *testing.T) {
	svc := &fakeSvc{diffs: reviewGallery(), draft: "Make a mess\n\nAdds hello.sh. Updates mine.txt and notes.txt.\n"}
	m := newReviewModel(t, svc)

	key(t, m, "A")
	if !m.rvDraft {
		t.Fatal("A must open the commit draft")
	}
	if !strings.Contains(m.rvTA.Value(), "Make a mess") {
		t.Errorf("draft not prefilled: %q", m.rvTA.Value())
	}
	if out := m.viewReview(); !strings.Contains(out, "ctrl+d") || !strings.Contains(out, "commit") {
		t.Errorf("draft box needs its keys spelled out:\n%s", out)
	}

	// esc abandons: staging kept, nothing committed.
	key(t, m, "esc")
	if m.rvDraft || len(svc.commits) != 0 {
		t.Fatalf("esc must abandon without committing, commits=%v", svc.commits)
	}
	if !strings.Contains(m.rvStatus, "staging kept") {
		t.Errorf("abandon should say the staging survives, got %q", m.rvStatus)
	}

	// A again, edit, ctrl+d commits the edited words.
	key(t, m, "A")
	m.rvTA.SetValue("Committed title\n\nEdited by hand.")
	key(t, m, "ctrl+d")
	if len(svc.commits) != 1 || !strings.HasPrefix(svc.commits[0], "Committed title") {
		t.Fatalf("commit must carry the edited draft, got %v", svc.commits)
	}
	if !strings.Contains(m.rvStatus, "abc1234") {
		t.Errorf("git's own output should surface, got %q", m.rvStatus)
	}
}

func TestReviewRestoreConfirmAndUndoAll(t *testing.T) {
	svc := &fakeSvc{diffs: reviewGallery()}
	m := newReviewModel(t, svc)

	// d on a file asks first; n cancels.
	key(t, m, "j") // hello.sh
	key(t, m, "d")
	if m.rvConfirm != "hello.sh" {
		t.Fatalf("d must ask before restoring, confirm=%q", m.rvConfirm)
	}
	if out := m.viewReview(); !strings.Contains(out, "hello.sh back to how it was") {
		t.Errorf("confirm question missing:\n%s", out)
	}
	key(t, m, "n")
	if len(svc.restored) != 0 {
		t.Fatal("n must cancel the restore")
	}

	// y restores just that file.
	key(t, m, "d")
	key(t, m, "y")
	if len(svc.restored) != 1 || len(svc.restored[0]) != 1 || svc.restored[0][0] != "hello.sh" {
		t.Fatalf("want a single-path restore, got %+v", svc.restored)
	}

	// u undoes the rest; the outside file survives, so the screen stays.
	key(t, m, "u")
	key(t, m, "y")
	if got := svc.restored[len(svc.restored)-1]; got != nil {
		t.Fatalf("u must call Restore(nil), got %v", got)
	}
	if m.screen != screenReview {
		t.Fatal("screen must stay open while a skipped file remains")
	}
	if !strings.Contains(m.rvStatus, "skipped 1") {
		t.Errorf("the skip must be reported, got %q", m.rvStatus)
	}
}

func TestReviewUndoAllClosesWhenClean(t *testing.T) {
	svc := &fakeSvc{diffs: []hive.FileDiff{
		{Path: "notes.txt", Status: "M", Patch: "@@ -1 +1 @@\n-a\n+b"},
	}}
	m := newReviewModel(t, svc)
	key(t, m, "u")
	key(t, m, "y")
	if m.screen != screenChat {
		t.Error("a clean blanket undo must close the review screen")
	}
}

func TestRequestChanges(t *testing.T) {
	svc := &fakeSvc{diffs: reviewGallery()}
	m := newReviewModel(t, svc)
	key(t, m, "r")
	if m.screen != screenChat {
		t.Fatal("r must land back in the conversation")
	}
	if m.reqBanner != "requesting changes on 3 files" {
		t.Errorf("banner wrong: %q", m.reqBanner)
	}
	if !strings.Contains(m.viewChat(), "requesting changes on 3 files") {
		t.Error("the banner must show above the composer")
	}
	if !strings.Contains(m.reqDiff, "+GAMMA") {
		t.Errorf("the diff must ride along, got %q", m.reqDiff)
	}
	if len(svc.restored)+len(svc.staged)+len(svc.commits) != 0 {
		t.Error("r must not restore, stage, or commit anything")
	}
}

func TestReviewMergeAsk(t *testing.T) {
	svc := &fakeSvc{diffs: reviewGallery(), draft: "Make a mess\n", branch: "hachi/make-a-mess",
		mergeRep: hive.MergeReport{Branch: "hachi/make-a-mess", Merged: true, Cleaned: true,
			Detail: "merged hachi/make-a-mess into main"}}
	m := newReviewModel(t, svc)
	m.sess.Branch = "hachi/make-a-mess"

	if out := m.viewReview(); !strings.Contains(out, "m merge into main") {
		t.Errorf("a worktree session's review needs the merge hint:\n%s", out)
	}

	// Commit on the branch, and the one question follows.
	key(t, m, "A")
	key(t, m, "ctrl+d")
	if m.rvMergeQ == "" {
		t.Fatal("committing on a branch must offer the merge")
	}
	if out := m.viewReview(); !strings.Contains(out, "Bring it into main now?") {
		t.Errorf("the ask must be visible in the footer:\n%s", out)
	}

	// Not yet is always safe, and m re-offers later.
	key(t, m, "n")
	if svc.merges != 0 {
		t.Fatal("n must not merge")
	}
	if !strings.Contains(m.rvStatus, "m brings it into main later") {
		t.Errorf("declining should say how to come back, got %q", m.rvStatus)
	}
	key(t, m, "m")
	if m.rvMergeQ == "" {
		t.Fatal("m must re-offer the merge")
	}

	// Yes merges, and git's sentence lands in the status line.
	key(t, m, "y")
	if svc.merges != 1 {
		t.Fatalf("y must call MergeBack once, got %d", svc.merges)
	}
	if !strings.Contains(m.rvStatus, "merged hachi/make-a-mess into main") {
		t.Errorf("the merge outcome must surface, got %q", m.rvStatus)
	}
}

func TestReviewMergeAskAbsentInPlace(t *testing.T) {
	svc := &fakeSvc{diffs: reviewGallery(), draft: "Make a mess\n"}
	m := newReviewModel(t, svc)

	if out := m.viewReview(); strings.Contains(out, "merge into main") {
		t.Errorf("an in-place session has no branch; no merge hint:\n%s", out)
	}
	key(t, m, "A")
	key(t, m, "ctrl+d")
	if m.rvMergeQ != "" {
		t.Fatal("no branch, no merge question")
	}
	key(t, m, "m")
	if m.rvMergeQ != "" {
		t.Fatal("m must do nothing without a branch")
	}
}
