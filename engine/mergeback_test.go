package engine_test

// Merge-back: the second half of the worktree story. A committed branch
// comes home through the user's checkout, cleanup follows only a clean
// merge, and the two refusals (dirty overlap, conflict) leave every ref
// and every user file exactly as they were.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tamnd/hachi/waggle"
)

func TestMergeBackCleansUp(t *testing.T) {
	repo := newRepo(t)
	e := newEngine(t)

	a, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stA := startScriptedTurn(t, e, a.ID)
	defer stA.finish()

	b, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stB := startScriptedTurn(t, e, b.ID)
	bi := sessionInfo(t, e, b.ID)
	if bi.Branch == "" {
		t.Fatal("session B must have upgraded to a worktree")
	}

	stB.edit("in_progress", "create", "notes.txt")
	write(t, filepath.Join(bi.Dir, "notes.txt"), []byte("honey\n"), 0o644)
	stB.edit("completed", "create", "notes.txt")
	stB.finish()

	if _, err := e.Stage(t.Context(), b.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Commit(t.Context(), b.ID, "Add notes\n"); err != nil {
		t.Fatal(err)
	}

	rep, err := e.MergeBack(t.Context(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Merged || !rep.Cleaned {
		t.Fatalf("expected a clean merge and cleanup, got %+v", rep)
	}
	if got := readFile(t, filepath.Join(repo, "notes.txt")); got != "honey\n" {
		t.Fatalf("the branch's work must land in the checkout, got %q", got)
	}
	if _, err := os.Lstat(bi.Dir); !os.IsNotExist(err) {
		t.Fatalf("the worktree must be removed after a clean merge, err=%v", err)
	}
	if out := git(t, repo, "branch", "--list", rep.Branch); strings.TrimSpace(out) != "" {
		t.Fatalf("the merged branch must be deleted, still see %q", out)
	}
	if list := git(t, repo, "worktree", "list"); strings.Count(list, "\n") != 0 {
		t.Fatalf("no extra worktrees may remain:\n%s", list)
	}

	// The session comes home: in place, no branch, and its next turn
	// re-anchors with a fresh baseline instead of the removed worktree.
	bi = sessionInfo(t, e, b.ID)
	if bi.Dir != repo && resolvedPath(t, bi.Dir) != resolvedPath(t, repo) {
		t.Fatalf("the session must move back to the checkout, got %s", bi.Dir)
	}
	if bi.Branch != "" {
		t.Fatalf("no branch may linger on the session, got %q", bi.Branch)
	}

	// The transcript says what happened, in hachi's voice.
	evs, err := e.Journal.Replay(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if last.Kind != waggle.KindFinding || last.Bee != "hachi" || !strings.Contains(string(last.Data), "merged") {
		t.Fatalf("the journal must record the merge, got %+v", last)
	}
}

func TestMergeBackRefusesDirtyOverlap(t *testing.T) {
	repo := newRepo(t)
	e := newEngine(t)

	a, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stA := startScriptedTurn(t, e, a.ID)
	defer stA.finish()

	b, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stB := startScriptedTurn(t, e, b.ID)
	bi := sessionInfo(t, e, b.ID)

	stB.edit("in_progress", "update", "README.md")
	appendTo(t, filepath.Join(bi.Dir, "README.md"), "from the branch\n")
	stB.edit("completed", "update", "README.md")
	stB.finish()
	if _, err := e.Stage(t.Context(), b.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Commit(t.Context(), b.ID, "Touch the readme\n"); err != nil {
		t.Fatal(err)
	}

	// The user's own uncommitted edit to the same file blocks the merge.
	appendTo(t, filepath.Join(repo, "README.md"), "the user's own line\n")
	before := git(t, repo, "rev-parse", "HEAD")

	rep, err := e.MergeBack(t.Context(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Merged || rep.Conflict {
		t.Fatalf("a dirty overlap must refuse before merging, got %+v", rep)
	}
	if len(rep.Blocked) != 1 || rep.Blocked[0] != "README.md" {
		t.Fatalf("the refusal must name the files, got %v", rep.Blocked)
	}
	if !strings.Contains(rep.Detail, "README.md") {
		t.Fatalf("the sentence must name the file, got %q", rep.Detail)
	}
	if after := git(t, repo, "rev-parse", "HEAD"); after != before {
		t.Fatal("a refusal must not move the checkout")
	}
	if got := readFile(t, filepath.Join(repo, "README.md")); !strings.Contains(got, "the user's own line") {
		t.Fatal("the user's uncommitted work must survive untouched")
	}
	if bi := sessionInfo(t, e, b.ID); bi.Branch == "" {
		t.Fatal("the branch must survive a refusal")
	}
}

func TestMergeBackAbortsConflict(t *testing.T) {
	repo := newRepo(t)
	e := newEngine(t)

	a, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stA := startScriptedTurn(t, e, a.ID)
	defer stA.finish()

	b, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stB := startScriptedTurn(t, e, b.ID)
	bi := sessionInfo(t, e, b.ID)

	stB.edit("in_progress", "update", "README.md")
	write(t, filepath.Join(bi.Dir, "README.md"), []byte("branch version\n"), 0o644)
	stB.edit("completed", "update", "README.md")
	stB.finish()
	if _, err := e.Stage(t.Context(), b.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Commit(t.Context(), b.ID, "Branch readme\n"); err != nil {
		t.Fatal(err)
	}

	// The checkout moves on with a committed, conflicting version.
	write(t, filepath.Join(repo, "README.md"), []byte("checkout version\n"), 0o644)
	git(t, repo, "commit", "-aqm", "checkout readme")
	before := git(t, repo, "rev-parse", "HEAD")

	rep, err := e.MergeBack(t.Context(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Conflict || rep.Merged {
		t.Fatalf("expected an aborted conflict, got %+v", rep)
	}
	if !strings.Contains(rep.Detail, "git merge "+rep.Branch) || !strings.Contains(rep.Detail, "git merge --abort") {
		t.Fatalf("the sentence must hand over the two commands, got %q", rep.Detail)
	}
	if after := git(t, repo, "rev-parse", "HEAD"); after != before {
		t.Fatal("an aborted merge must not move the checkout")
	}
	if _, err := os.Lstat(filepath.Join(repo, ".git", "MERGE_HEAD")); !os.IsNotExist(err) {
		t.Fatal("the merge must be fully aborted, MERGE_HEAD still present")
	}
	if got := readFile(t, filepath.Join(repo, "README.md")); got != "checkout version\n" {
		t.Fatalf("the checkout's file must be untouched after the abort, got %q", got)
	}
	if bi := sessionInfo(t, e, b.ID); bi.Branch == "" {
		t.Fatal("the branch must survive an aborted merge")
	}
}

func TestMergeBackNothingCommitted(t *testing.T) {
	repo := newRepo(t)
	e := newEngine(t)

	a, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stA := startScriptedTurn(t, e, a.ID)
	defer stA.finish()

	b, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stB := startScriptedTurn(t, e, b.ID)
	stB.finish()
	if bi := sessionInfo(t, e, b.ID); bi.Branch == "" {
		t.Fatal("session B must have upgraded to a worktree")
	}

	rep, err := e.MergeBack(t.Context(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Merged || rep.Conflict || len(rep.Blocked) > 0 {
		t.Fatalf("nothing to merge must be a quiet no, got %+v", rep)
	}
	if !strings.Contains(rep.Detail, "no commits") {
		t.Fatalf("the sentence must say why, got %q", rep.Detail)
	}
}

func TestMergeBackRefusesInPlaceSession(t *testing.T) {
	repo := newRepo(t)
	e := newEngine(t)
	a, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.MergeBack(t.Context(), a.ID); err == nil {
		t.Fatal("an in-place session has no branch to merge; expected an error")
	}
}

func resolvedPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}
