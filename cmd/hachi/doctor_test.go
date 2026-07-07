package main

// doctor's audit against a staged mess: one live session that must stay
// untouched, one stale baseline ref whose session is gone, one worktree
// nothing references, and the branch that outlives it.

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/hachi/journal"
)

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitIn(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func TestDoctorListsAndFixesLeftovers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	home := t.TempDir()
	t.Setenv("HACHI_HOME", home)
	// A neutral cwd keeps the dev checkout's own refs out of the audit.
	t.Chdir(t.TempDir())

	repo := t.TempDir()
	testGit(t, repo, "init", "-q")
	testGit(t, repo, "config", "user.email", "test@hachi.local")
	testGit(t, repo, "config", "user.name", "hachi test")
	testGit(t, repo, "commit", "-q", "--allow-empty", "-m", "root")
	head := testGit(t, repo, "rev-parse", "HEAD")

	// The live session anchors the repo in the audit and must survive it.
	j, err := journal.NewFiles(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.SaveMeta(journal.Meta{ID: "alive", Dir: repo, Brain: "codex", Created: time.Now(), Updated: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()

	// A ref whose session is gone, and a worktree nothing references.
	testGit(t, repo, "update-ref", "refs/hachi/baseline/ghost", head)
	orphan := filepath.Join(home, "worktrees", "repo-000000", "orphan")
	testGit(t, repo, "worktree", "add", "-q", orphan, "-b", "hachi/orphan")

	run := func(args ...string) string {
		var buf bytes.Buffer
		cmd := doctorCmd()
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("doctor %v: %v\n%s", args, err, buf.String())
		}
		return buf.String()
	}

	out := run()
	for _, want := range []string{
		"1 session",
		"stale ref refs/hachi/baseline/ghost",
		"orphaned worktree " + orphan,
		"branch hachi/orphan",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor must report %q:\n%s", want, out)
		}
	}

	out = run("--fix")
	if !strings.Contains(out, "removed stale ref") || !strings.Contains(out, "removed orphaned worktree") {
		t.Fatalf("--fix must say what it removed:\n%s", out)
	}
	if refs := testGit(t, repo, "for-each-ref", "refs/hachi/"); refs != "" {
		t.Fatalf("the stale ref must be gone:\n%s", refs)
	}
	if wl := testGit(t, repo, "worktree", "list"); strings.Contains(wl, "orphan") {
		t.Fatalf("the orphaned worktree must be gone:\n%s", wl)
	}
	// The branch is the user's to delete; --fix names it and leaves it.
	if !strings.Contains(out, "branch hachi/orphan") {
		t.Fatalf("--fix must still name the surviving branch:\n%s", out)
	}
	if br := testGit(t, repo, "branch", "--list", "hachi/orphan"); !strings.Contains(br, "hachi/orphan") {
		t.Fatal("--fix must never delete a branch")
	}

	// A clean home has nothing to say.
	testGit(t, repo, "branch", "-D", "hachi/orphan")
	out = run()
	if !strings.Contains(out, "none") {
		t.Fatalf("a tidy hive reads as none:\n%s", out)
	}
}
