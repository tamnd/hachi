package engine_test

// The accept path: stage exactly the session's work, draft a message
// worth editing, commit only what the human confirmed, and keep undo
// honest about staged state. Every test runs real git against a real
// tree; the assertions are git's own answers, not the engine's.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/tamnd/hachi/waggle"
)

// stageAndSets runs blanket Stage and returns what hachi staged next to
// what git now holds in the index.
func stagedNames(t *testing.T, dir string) []string {
	t.Helper()
	out := git(t, dir, "diff", "--cached", "--name-only")
	if out == "" {
		return nil
	}
	names := strings.Split(out, "\n")
	sort.Strings(names)
	return names
}

func TestStageGit(t *testing.T) {
	dir := setupMessRepo(t)
	e := newEngine(t)
	info, err := e.Open(t.Context(), "", dir, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Baseline(t.Context(), info.ID); err != nil {
		t.Fatal(err)
	}
	runMess(t, dir)

	staged, err := e.Stage(context.Background(), info.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(staged)
	want := []string{"blob.bin", "created.txt", "link.txt", "script.sh", "sub/deep.txt", "tracked.txt", "untracked.txt"}
	if !slices.Equal(staged, want) {
		t.Errorf("staged set:\n want %v\n got  %v", want, staged)
	}
	// staged.txt was the user's own pre-session staging; hachi must not
	// have touched it, and it must still sit staged.
	if slices.Contains(staged, "staged.txt") {
		t.Error("staged.txt was not session work, must not be in the staged set")
	}
	inIndex := stagedNames(t, dir)
	if !slices.Contains(inIndex, "staged.txt") {
		t.Error("the user's own staged.txt fell out of the index")
	}
	for _, p := range want {
		if !slices.Contains(inIndex, p) {
			t.Errorf("%s not in the git index after Stage", p)
		}
	}
	// Staging twice must be a quiet no-op: A runs a blanket Stage after
	// a already did, and a staged deletion makes plain git add fail.
	again, err := e.Stage(context.Background(), info.ID, nil)
	if err != nil {
		t.Fatalf("second Stage must not error: %v", err)
	}
	sort.Strings(again)
	if !slices.Equal(again, want) {
		t.Errorf("second Stage drifted:\n want %v\n got  %v", want, again)
	}
	// Changes must now carry the accepted-mark.
	diffs, err := e.Changes(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range byPath(diffs) {
		if !d.Staged {
			t.Errorf("%s should read as staged in Changes", d.Path)
		}
	}
}

func TestCommitDraftAndCommit(t *testing.T) {
	dir := setupMessRepo(t)
	e := newEngine(t)
	info, err := e.Open(t.Context(), "", dir, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	st := startScriptedTurn(t, e, info.ID) // Send("make a mess") names the session
	runMess(t, dir)
	st.finish()
	if err := e.Stop(t.Context(), info.ID); err != nil { // wait out the turn so meta is saved
		t.Fatal(err)
	}

	if _, err := e.Stage(context.Background(), info.ID, nil); err != nil {
		t.Fatal(err)
	}
	draft, err := e.CommitDraft(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(draft, "\n", 2)[0]
	if first != "Make a mess" {
		t.Errorf("draft title should come from the ask, got %q", first)
	}
	if len(first) >= 60 {
		t.Errorf("draft title too long: %q", first)
	}
	for _, want := range []string{"Adds created.txt", "Deletes sub/deep.txt", "Updates"} {
		if !strings.Contains(draft, want) {
			t.Errorf("draft body misses %q:\n%s", want, draft)
		}
	}
	if strings.Contains(draft, "- ") {
		t.Errorf("draft body must be prose, not bullets:\n%s", draft)
	}

	// The human edits the draft; the commit must carry their words.
	message := "Make the planned mess\n\nEvery mutation class in one pass, on purpose.\n"
	out, err := e.Commit(context.Background(), info.ID, message)
	if err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	if got := git(t, dir, "log", "-1", "--format=%B"); strings.TrimSpace(got) != strings.TrimSpace(message) {
		t.Errorf("commit message drifted:\n want %q\n got  %q", message, got)
	}
	if !strings.Contains(out, "Make the planned mess") {
		t.Errorf("git's own output should come back, got %q", out)
	}
	// The user's own pre-session staging must survive the commit: still
	// staged, not committed. A whole-index commit would have swept it in.
	if got := git(t, dir, "log", "-1", "--name-only", "--format="); strings.Contains(got, "staged.txt") {
		t.Errorf("the user's staged.txt was committed by hachi:\n%s", got)
	}
	if got := git(t, dir, "diff", "--cached", "--name-only"); !strings.Contains(got, "staged.txt") {
		t.Errorf("staged.txt should still sit staged after hachi's commit, index holds:\n%s", got)
	}
	// The session's own work must all be in the commit and gone from
	// pending state.
	committed := git(t, dir, "log", "-1", "--name-only", "--format=")
	for _, want := range []string{"created.txt", "tracked.txt", "sub/deep.txt", "untracked.txt"} {
		if !strings.Contains(committed, want) {
			t.Errorf("%s missing from the commit:\n%s", want, committed)
		}
	}
	// Replay must show the commit happened.
	events, err := e.Journal.Replay(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	var marked bool
	for _, ev := range events {
		if ev.Kind == waggle.KindFinding && strings.Contains(string(ev.Data), "committed") {
			marked = true
		}
	}
	if !marked {
		t.Error("no transcript marker for the commit")
	}
}

// InRepo picks the review rendering: sentence view outside a repo,
// file tree inside one.
func TestOpenReportsInRepo(t *testing.T) {
	e := newEngine(t)
	repo, err := e.Open(t.Context(), "", setupMessRepo(t), "fake")
	if err != nil {
		t.Fatal(err)
	}
	if !repo.InRepo {
		t.Error("a git repo must report InRepo")
	}
	plain, err := e.Open(t.Context(), "", t.TempDir(), "fake")
	if err != nil {
		t.Fatal(err)
	}
	if plain.InRepo {
		t.Error("a plain folder must not report InRepo")
	}
}

func TestCommitRefusedOutsideGit(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	e := newEngine(t)
	info, err := e.Open(t.Context(), "", dir, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CommitDraft(context.Background(), info.ID); err == nil {
		t.Error("CommitDraft must refuse a non-git folder")
	}
	if _, err := e.Commit(context.Background(), info.ID, "x"); err == nil {
		t.Error("Commit must refuse a non-git folder")
	}
	if _, err := e.Commit(context.Background(), info.ID, "  \n"); err == nil {
		t.Error("Commit must refuse an empty message")
	}
}

// Stage then undo everything: the index must return to its baseline
// state too, or the index would hold content the tree no longer has.
func TestRestoreUnstagesFirst(t *testing.T) {
	dir := setupMessRepo(t)
	wantStatus := gitStatusZ(t, dir)
	wantHashes := hashTree(t, dir)

	e := newEngine(t)
	info, err := e.Open(t.Context(), "", dir, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Baseline(t.Context(), info.ID); err != nil {
		t.Fatal(err)
	}
	runMess(t, dir)
	if _, err := e.Stage(context.Background(), info.ID, nil); err != nil {
		t.Fatal(err)
	}

	rep, err := e.RestoreAll(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Skipped) != 0 {
		t.Errorf("nothing should be skipped, got %+v", rep.Skipped)
	}
	if got := gitStatusZ(t, dir); got != wantStatus {
		t.Errorf("git status drifted (index not unstaged?):\n want %q\n got  %q", wantStatus, got)
	}
	if diff := compareHashes(wantHashes, hashTree(t, dir)); diff != "" {
		t.Errorf("tree not byte-identical:\n%s", diff)
	}
}

func TestRestoreSinglePath(t *testing.T) {
	dir := setupMessRepo(t)
	e := newEngine(t)
	info, err := e.Open(t.Context(), "", dir, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Baseline(t.Context(), info.ID); err != nil {
		t.Fatal(err)
	}
	runMess(t, dir)

	rep, err := e.Restore(context.Background(), info.ID, []string{"tracked.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Restored, []string{"tracked.txt"}) {
		t.Fatalf("want only tracked.txt restored, got %+v", rep)
	}
	data, err := os.ReadFile(filepath.Join(dir, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "line1\nline2\ndirty\n" {
		t.Errorf("tracked.txt not back to its baseline bytes: %q", data)
	}
	// The rest of the mess must still be there.
	if _, err := os.Stat(filepath.Join(dir, "created.txt")); err != nil {
		t.Error("created.txt should survive a single-path restore")
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "deep.txt")); !os.IsNotExist(err) {
		t.Error("sub/deep.txt should stay deleted after a single-path restore")
	}
}

// Non-git keep: Stage narrows Undo, and only naming the path widens it
// back.
func TestNonGitKeepThenUndo(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "text.txt"), []byte("original\n"), 0o644)

	e := newEngine(t)
	info, err := e.Open(t.Context(), "", dir, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	st := startScriptedTurn(t, e, info.ID)
	st.edit("in_progress", "update", "text.txt")
	write(t, filepath.Join(dir, "text.txt"), []byte("agent\n"), 0o644)
	st.edit("completed", "update", "text.txt")
	st.edit("in_progress", "add", "new.txt")
	write(t, filepath.Join(dir, "new.txt"), []byte("new\n"), 0o644)
	st.edit("completed", "add", "new.txt")
	st.finish()

	staged, err := e.Stage(context.Background(), info.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(staged)
	if !slices.Equal(staged, []string{"new.txt", "text.txt"}) {
		t.Fatalf("keep should cover both files, got %v", staged)
	}

	rep, err := e.RestoreAll(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Restored) != 0 || len(rep.Skipped) != 2 {
		t.Fatalf("kept files must survive blanket undo, got %+v", rep)
	}
	for _, s := range rep.Skipped {
		if s.Reason != "you kept it" {
			t.Errorf("skip reason should say the user kept it, got %q", s.Reason)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "text.txt")); string(data) != "agent\n" {
		t.Errorf("kept file was restored anyway: %q", data)
	}

	// Naming the path is the confirmation.
	rep, err = e.Restore(context.Background(), info.ID, []string{"text.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Restored, []string{"text.txt"}) {
		t.Fatalf("explicit restore must override keep, got %+v", rep)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "text.txt")); string(data) != "original\n" {
		t.Errorf("text.txt not back to original: %q", data)
	}
}
