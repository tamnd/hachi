package tui

// The sentence view's promises: plain words only, buttons that map to
// the same engine verbs, and the one extra sentence before a non-git
// keep narrows Undo.

import (
	"strings"
	"testing"

	"github.com/tamnd/hachi/hive"
)

// The words locked out of the sentence view. "diff" would also catch
// lipgloss noise in styles, so the check runs on the rendered text.
var gitWords = []string{"stage", "commit", "index", "HEAD", "branch", "repository", "diff", "hunk", "untracked"}

func TestSentenceDefaultAndVocabulary(t *testing.T) {
	svc := &fakeSvc{diffs: reviewGallery(), nonGit: true}
	m := newReviewModel(t, svc)
	if !m.rvPlain {
		t.Fatal("a session outside a git repo must land in the sentence view")
	}
	out := m.viewReview()
	for _, want := range []string{
		"what changed", "Edited 3 files in your project.",
		"Created hello.sh",
		"You also changed mine.txt yourself, so hachi will leave it alone",
		"Added 1 line to notes.txt",
		"Keep these changes", "Undo everything", "show the code",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sentence view misses %q:\n%s", want, out)
		}
	}
	for _, word := range gitWords {
		if strings.Contains(strings.ToLower(out), word) {
			t.Errorf("the sentence view must never say %q:\n%s", word, out)
		}
	}
}

func TestSentenceDefaultInRepo(t *testing.T) {
	m := newReviewModel(t, &fakeSvc{diffs: reviewGallery()})
	if m.rvPlain {
		t.Fatal("a session in a git repo must land on the file tree")
	}
	key(t, m, "s")
	if !m.rvPlain {
		t.Fatal("s must flip to the sentence view")
	}
	// The toggle sticks across visits.
	key(t, m, "q")
	_, cmd := m.openReview()
	pump(t, m, cmd)
	if !m.rvPlain {
		t.Error("the chosen view must survive reopening review")
	}
}

func TestFileSentences(t *testing.T) {
	cases := []struct {
		f    hive.FileDiff
		want string
	}{
		{hive.FileDiff{Path: "a.txt", Status: "A"}, "Created a.txt"},
		{hive.FileDiff{Path: "b.txt", Status: "D"}, "Deleted b.txt"},
		{hive.FileDiff{Path: "c.txt", Status: "M", Patch: "@@\n+one\n+two"}, "Added 2 lines to c.txt"},
		{hive.FileDiff{Path: "d.txt", Status: "M", Patch: "@@\n-gone"}, "Removed 1 line from d.txt"},
		{hive.FileDiff{Path: "e.txt", Status: "M", Patch: "@@\n-x\n-y\n-z\n+p\n+q"}, "Changed 3 lines in e.txt"},
		{hive.FileDiff{Path: "f.bin", Status: "M", Binary: true}, "Changed f.bin"},
		{hive.FileDiff{Path: "g.txt", Status: "M", Outside: true}, "You also changed g.txt yourself, so hachi will leave it alone"},
	}
	for _, c := range cases {
		if got := fileSentence(c.f); got != c.want {
			t.Errorf("fileSentence(%s):\n want %q\n got  %q", c.f.Path, c.want, got)
		}
	}
}

func TestSentenceNoUndoWarning(t *testing.T) {
	diffs := append(reviewGallery(), hive.FileDiff{
		Path: "video.mp4", Status: "M", NoUndo: true,
		Note: "changed, but the old version was too big to keep a copy of",
	})
	m := newReviewModel(t, &fakeSvc{diffs: diffs, nonGit: true})
	out := m.viewReview()
	if !strings.Contains(out, "Undo will not cover it") {
		t.Errorf("a file without a saved copy needs its warning:\n%s", out)
	}
}

func TestSentenceKeepButtonInRepo(t *testing.T) {
	svc := &fakeSvc{diffs: reviewGallery()}
	m := newReviewModel(t, svc)
	key(t, m, "s")
	key(t, m, "enter") // keep is the default button; in a repo no question runs
	if len(svc.staged) != 1 || svc.staged[0] != nil {
		t.Fatalf("Keep must be a blanket Stage(nil), got %+v", svc.staged)
	}
	if !strings.Contains(m.rvStatus, "2 files kept") {
		t.Errorf("the sentence view never says staged, got %q", m.rvStatus)
	}
}

func TestSentenceUndoButton(t *testing.T) {
	svc := &fakeSvc{diffs: reviewGallery(), nonGit: true}
	m := newReviewModel(t, svc)
	key(t, m, "tab")
	if m.rvBtn != 1 {
		t.Fatal("tab must move focus to the Undo button")
	}
	key(t, m, "enter")
	if !strings.Contains(m.viewReview(), "Undo everything this session did? y/n") {
		t.Fatalf("Undo must ask first:\n%s", m.viewReview())
	}
	key(t, m, "y")
	if len(svc.restored) != 1 || svc.restored[0] != nil {
		t.Fatalf("Undo everything must be a blanket Restore(nil), got %+v", svc.restored)
	}
	// The outside-edited file was skipped, so the screen stays up and
	// says so.
	if m.screen != screenReview {
		t.Error("a partial undo must not close the screen")
	}
	if !strings.Contains(m.rvStatus, "skipped") {
		t.Errorf("the skip must be reported, got %q", m.rvStatus)
	}
}

func TestNonGitFirstKeepAsks(t *testing.T) {
	svc := &fakeSvc{diffs: reviewGallery(), nonGit: true}
	m := newReviewModel(t, svc)
	key(t, m, "enter") // keep
	if len(svc.staged) != 0 {
		t.Fatal("the first non-git keep must ask before touching anything")
	}
	if !strings.Contains(m.viewReview(), "Undo stops covering") {
		t.Fatalf("the narrows-undo sentence is missing:\n%s", m.viewReview())
	}
	key(t, m, "n")
	if len(svc.staged) != 0 {
		t.Fatal("n must cancel the keep")
	}
	key(t, m, "enter")
	key(t, m, "y")
	if len(svc.staged) != 1 {
		t.Fatalf("y must run the keep, got %+v", svc.staged)
	}
	// Once answered, the question never comes back this run.
	key(t, m, "enter")
	if len(svc.staged) != 2 {
		t.Fatalf("the second keep must run without asking, got %+v", svc.staged)
	}
}

func TestNonGitStageAllViaA(t *testing.T) {
	svc := &fakeSvc{diffs: reviewGallery(), nonGit: true}
	m := newReviewModel(t, svc)
	key(t, m, "s") // over to the file tree; A must degrade there too
	key(t, m, "A")
	if len(svc.commits) != 0 {
		t.Fatal("A outside a repo must never reach Commit")
	}
	key(t, m, "y") // the first-keep question
	if len(svc.staged) != 1 || svc.staged[0] != nil {
		t.Fatalf("A must degrade to a blanket keep, got %+v", svc.staged)
	}
	if !strings.Contains(m.rvStatus, "not a git repo") {
		t.Errorf("the degrade needs its note, got %q", m.rvStatus)
	}
	if !strings.Contains(m.rvStatus, "kept") {
		t.Errorf("outside a repo the verb is kept, got %q", m.rvStatus)
	}
}
