package engine_test

// Delete: the destructive verb, tested against its own promises. The
// journal directory, the baseline ref, and the private worktree all go;
// a branch whose commits live nowhere else stays and gets named; a
// running turn is stopped first, never orphaned.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeleteKeepsCommittedBranch(t *testing.T) {
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
	if bi = sessionInfo(t, e, b.ID); !bi.Committed {
		t.Fatal("Commit must mark the session committed")
	}

	kept, err := e.Delete(t.Context(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(kept, bi.Branch) {
		t.Fatalf("the sentence must name the surviving branch, got %q", kept)
	}

	// The worktree and the journal go; the branch stays because its
	// commit is nowhere else.
	if _, err := os.Lstat(bi.Dir); !os.IsNotExist(err) {
		t.Fatalf("the worktree must be removed, err=%v", err)
	}
	if _, err := os.Stat(e.Journal.SessionDir(b.ID)); !os.IsNotExist(err) {
		t.Fatalf("the journal directory must be removed, err=%v", err)
	}
	if out := git(t, repo, "branch", "--list", bi.Branch); !strings.Contains(out, bi.Branch) {
		t.Fatalf("the unmerged branch must survive, branch list: %q", out)
	}
	if refs := git(t, repo, "for-each-ref", "refs/hachi/"); strings.Contains(refs, string(b.ID)) {
		t.Fatalf("the baseline ref must be gone:\n%s", refs)
	}

	list, err := e.Sessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.ID == b.ID {
			t.Fatal("a deleted session must not come back in the list")
		}
	}
}

func TestDeleteDropsUncommittedBranch(t *testing.T) {
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

	// Uncommitted leftovers in the worktree: the confirmed delete covers
	// them, and a branch with no commits of its own has nothing to keep.
	stB.edit("in_progress", "create", "notes.txt")
	write(t, filepath.Join(bi.Dir, "notes.txt"), []byte("honey\n"), 0o644)
	stB.edit("completed", "create", "notes.txt")
	stB.finish()

	kept, err := e.Delete(t.Context(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept != "" {
		t.Fatalf("nothing survives a commitless delete, got %q", kept)
	}
	if _, err := os.Lstat(bi.Dir); !os.IsNotExist(err) {
		t.Fatalf("the worktree must be removed, err=%v", err)
	}
	if out := git(t, repo, "branch", "--list", bi.Branch); strings.TrimSpace(out) != "" {
		t.Fatalf("a branch with no commits must be deleted, still see %q", out)
	}
	if list := git(t, repo, "worktree", "list"); strings.Count(list, "\n") != 0 {
		t.Fatalf("no extra worktrees may remain:\n%s", list)
	}
}

func TestDeleteInPlaceDropsBaselineRef(t *testing.T) {
	repo := newRepo(t)
	// A dirty tracked file at capture forces the stash commit that gets
	// pinned under refs/hachi/, the thing delete must unpin.
	write(t, filepath.Join(repo, "README.md"), []byte("hello\nwip\n"), 0o644)
	e := newEngine(t)

	info, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	st := startScriptedTurn(t, e, info.ID)
	st.edit("in_progress", "create", "agent.txt")
	write(t, filepath.Join(repo, "agent.txt"), []byte("agent\n"), 0o644)
	st.edit("completed", "create", "agent.txt")
	st.finish()

	ref := "refs/hachi/baseline/" + string(info.ID)
	if refs := git(t, repo, "for-each-ref", "refs/hachi/"); !strings.Contains(refs, ref) {
		t.Fatalf("the baseline must be pinned before delete:\n%s", refs)
	}

	kept, err := e.Delete(t.Context(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept != "" {
		t.Fatalf("an in-place delete leaves nothing behind, got %q", kept)
	}
	if refs := git(t, repo, "for-each-ref", "refs/hachi/"); strings.Contains(refs, ref) {
		t.Fatalf("the baseline ref must be gone:\n%s", refs)
	}
	if _, err := os.Stat(e.Journal.SessionDir(info.ID)); !os.IsNotExist(err) {
		t.Fatalf("the journal directory must be removed, err=%v", err)
	}
	// Delete removes hachi's records, never the tree: the user's wip and
	// the agent's unreviewed file both stay exactly as they were.
	if got := readFile(t, filepath.Join(repo, "README.md")); got != "hello\nwip\n" {
		t.Fatalf("delete must not touch the user's wip, got %q", got)
	}
	if got := readFile(t, filepath.Join(repo, "agent.txt")); got != "agent\n" {
		t.Fatalf("delete must not restore or remove the agent's file, got %q", got)
	}
}

func TestDeleteStopsRunningTurn(t *testing.T) {
	dir := t.TempDir()
	e := newEngine(t)
	info, err := e.Open(t.Context(), "", dir, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	st := startScriptedTurn(t, e, info.ID)

	done := make(chan error, 1)
	go func() {
		_, err := e.Delete(context.Background(), info.ID)
		done <- err
	}()
	// Delete stops the turn and waits for the drain; the scripted stream
	// ends when its channel closes, standing in for the adapter obeying
	// the stop.
	close(st.stream.ch)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delete never came back from stopping the turn")
	}
	if _, err := os.Stat(e.Journal.SessionDir(info.ID)); !os.IsNotExist(err) {
		t.Fatalf("the journal directory must be removed, err=%v", err)
	}
}

func TestDeleteUnknownSession(t *testing.T) {
	e := newEngine(t)
	if _, err := e.Delete(t.Context(), "nope"); err == nil {
		t.Fatal("deleting an unknown session must fail, not invent one")
	}
}
