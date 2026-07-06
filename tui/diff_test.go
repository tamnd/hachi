package tui

import (
	"strings"
	"testing"

	"github.com/tamnd/hachi/hive"
)

var diffGallery = []hive.FileDiff{
	{Path: "notes.txt", Status: "M", Patch: "@@ -1,2 +1,3 @@\n alpha\n beta\n+GAMMA"},
	{Path: "hello.sh", Status: "A", Patch: "@@ -0,0 +1,2 @@\n+#!/bin/sh\n+echo hi"},
	{Path: "old.txt", Status: "D", Patch: "@@ -1 +0,0 @@\n-obsolete"},
	{Path: "assets/logo.png", Status: "M", Binary: true, Note: "binary file changed (2.1 KB → 2.3 KB)"},
	{Path: "mine.txt", Status: "M", Outside: true, Patch: "@@ -1 +1 @@\n-agent\n+mine"},
}

func newDiffModel(t *testing.T) *model {
	t.Helper()
	m := newModel(nil, Options{Dir: "/tmp", Brain: "codex"})
	m.w, m.h = 100, 30
	m.draft = false
	m.layout()
	return m
}

func TestDiffScreenRenders(t *testing.T) {
	m := newDiffModel(t)
	m.screen = screenDiff
	m.applyDiff(diffMsg{files: diffGallery})

	out := m.viewDiff()
	for _, want := range []string{"changes so far", "5 files", "notes.txt", "+GAMMA", "you changed this after",
		"binary file changed (2.1 KB → 2.3 KB)", "refresh"} {
		if !strings.Contains(out, want) {
			t.Errorf("diff screen misses %q:\n%s", want, out)
		}
	}
	// One section mark per file, in content order.
	if len(m.diffMarks) != len(diffGallery) {
		t.Fatalf("want %d section marks, got %d", len(diffGallery), len(m.diffMarks))
	}
	for i := 1; i < len(m.diffMarks); i++ {
		if m.diffMarks[i] <= m.diffMarks[i-1] {
			t.Errorf("marks must ascend, got %v", m.diffMarks)
		}
	}
}

func TestDiffScreenTally(t *testing.T) {
	adds, dels := patchTally(diffGallery[0].Patch)
	if adds != 1 || dels != 0 {
		t.Errorf("notes.txt tally: want +1 -0, got +%d -%d", adds, dels)
	}
	adds, dels = patchTally(diffGallery[2].Patch)
	if adds != 0 || dels != 1 {
		t.Errorf("old.txt tally: want +0 -1, got +%d -%d", adds, dels)
	}
	if a, d := patchTally(""); a != 0 || d != 0 {
		t.Errorf("empty patch must tally zero, got +%d -%d", a, d)
	}
}

func TestDiffScreenEmptyAndError(t *testing.T) {
	m := newDiffModel(t)
	m.screen = screenDiff
	m.applyDiff(diffMsg{files: nil})
	if out := m.viewDiff(); !strings.Contains(out, "no changes yet") {
		t.Errorf("empty diff needs the no-changes line:\n%s", out)
	}
	m.applyDiff(diffMsg{err: errFake("baseline missing")})
	if out := m.viewDiff(); !strings.Contains(out, "baseline missing") {
		t.Errorf("errors must surface on screen:\n%s", out)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
