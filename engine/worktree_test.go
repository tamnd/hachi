package engine_test

// The S4 silent upgrade: a second session starting to write in a busy
// repo moves into a private worktree before its first turn, the first
// session never moves, and the only trace in the conversation is one
// quiet line. Two idle sessions in one repo trigger nothing.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tamnd/hachi/engine"
	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

// newRepo builds a one-commit repo the tests can collide in.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "test@hachi.local")
	git(t, dir, "config", "user.name", "hachi test")
	write(t, filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "root")
	return dir
}

func sessionInfo(t *testing.T, e *engine.Engine, id waggle.SessionID) hive.SessionInfo {
	t.Helper()
	info, err := e.Open(t.Context(), id, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestSecondSessionGetsWorktree(t *testing.T) {
	repo := newRepo(t)
	e := newEngine(t)

	a, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stA := startScriptedTurn(t, e, a.ID)
	defer stA.finish()

	// Dirt in the user's checkout must not leak into the private copy:
	// the worktree grows from HEAD, not from the tree.
	appendTo(t, filepath.Join(repo, "README.md"), "dirty\n")

	b, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stB := startScriptedTurn(t, e, b.ID)
	defer stB.finish()

	bi := sessionInfo(t, e, b.ID)
	if bi.Branch != "hachi/make-a-mess" {
		t.Fatalf("second session must land on its own branch, got %q", bi.Branch)
	}
	if bi.Dir == repo || !strings.Contains(bi.Dir, string(filepath.Separator)+"worktrees"+string(filepath.Separator)) {
		t.Fatalf("second session must work in a private copy, still in %s", bi.Dir)
	}
	if data := readFile(t, filepath.Join(bi.Dir, "README.md")); strings.Contains(data, "dirty") {
		t.Fatalf("the private copy must start from the last commit, saw uncommitted dirt: %q", data)
	}

	ai := sessionInfo(t, e, a.ID)
	if ai.Dir != repo || ai.Branch != "" {
		t.Fatalf("the first session must never move, got dir=%s branch=%q", ai.Dir, ai.Branch)
	}

	bm, err := e.Baseline(t.Context(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bm.CleanAtBaseline {
		t.Fatalf("a fresh worktree must hit the clean baseline fast path, got %+v", bm)
	}
	if bm.Root == repo {
		t.Fatal("the second session's baseline must anchor in the worktree, not the repo")
	}

	// The one visible trace: a quiet line before the first message,
	// naming the session it is staying out of the way of.
	evs, err := e.Journal.Replay(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 || evs[0].Kind != waggle.KindFinding || evs[0].Bee != "hachi" {
		t.Fatalf("the transcript must open with hachi's quiet line, got %+v", evs[:min(len(evs), 2)])
	}
	var msg waggle.Message
	if err := json.Unmarshal(evs[0].Data, &msg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.Text, "private copy") || !strings.Contains(msg.Text, `"make a mess"`) {
		t.Fatalf("the quiet line must say why and name the other session, got %q", msg.Text)
	}

	if list := git(t, repo, "worktree", "list"); strings.Count(list, "\n") != 1 {
		t.Fatalf("expected exactly one extra worktree:\n%s", list)
	}
	git(t, repo, "show-ref", "--verify", "refs/heads/hachi/make-a-mess")
}

func TestIdleSessionsStayInPlace(t *testing.T) {
	repo := newRepo(t)
	e := newEngine(t)

	// Open but never send: an idle session with no changes is not a
	// collision, and the second session works in place.
	if _, err := e.Open(t.Context(), "", repo, "scripted"); err != nil {
		t.Fatal(err)
	}
	b, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	st := startScriptedTurn(t, e, b.ID)
	defer st.finish()

	bi := sessionInfo(t, e, b.ID)
	if bi.Dir != repo || bi.Branch != "" {
		t.Fatalf("no collision means no upgrade, got dir=%s branch=%q", bi.Dir, bi.Branch)
	}
}

func TestUnreviewedChangesTriggerUpgrade(t *testing.T) {
	repo := newRepo(t)
	e := newEngine(t)

	// Session A runs a turn that edits a file and finishes. Nothing was
	// reviewed, so the folder still belongs to A.
	a, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stA := startScriptedTurn(t, e, a.ID)
	stA.edit("in_progress", "modify", filepath.Join(repo, "README.md"))
	appendTo(t, filepath.Join(repo, "README.md"), "agent work\n")
	stA.edit("completed", "modify", filepath.Join(repo, "README.md"))
	stA.finish()

	b, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stB := startScriptedTurn(t, e, b.ID)
	defer stB.finish()

	bi := sessionInfo(t, e, b.ID)
	if bi.Branch == "" {
		t.Fatal("unreviewed changes in the repo must push the newcomer into a worktree")
	}
}

func TestWorktreeSlugDedup(t *testing.T) {
	repo := newRepo(t)
	e := newEngine(t)

	a, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stA := startScriptedTurn(t, e, a.ID)
	defer stA.finish()

	var branches []string
	for range 2 {
		s, err := e.Open(t.Context(), "", repo, "scripted")
		if err != nil {
			t.Fatal(err)
		}
		st := startScriptedTurn(t, e, s.ID)
		defer st.finish()
		branches = append(branches, sessionInfo(t, e, s.ID).Branch)
	}
	if branches[0] != "hachi/make-a-mess" || branches[1] != "hachi/make-a-mess-2" {
		t.Fatalf("same title must dedup with a numeric suffix, got %v", branches)
	}
}

func TestSubdirSessionCollidesByRoot(t *testing.T) {
	repo := newRepo(t)
	sub := filepath.Join(repo, "pkg")
	write(t, filepath.Join(sub, "code.go"), []byte("package pkg\n"), 0o644)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "sub")
	e := newEngine(t)

	a, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stA := startScriptedTurn(t, e, a.ID)
	defer stA.finish()

	// Detection is by repo root, not cwd: a session down in a subdir
	// collides with one at the root, and keeps its spot in the copy.
	b, err := e.Open(t.Context(), "", sub, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stB := startScriptedTurn(t, e, b.ID)
	defer stB.finish()

	bi := sessionInfo(t, e, b.ID)
	if bi.Branch == "" {
		t.Fatal("a subdir session must still collide by repo root")
	}
	if filepath.Base(bi.Dir) != "pkg" {
		t.Fatalf("the session must keep its place relative to the root, got %s", bi.Dir)
	}
}

func TestUpgradedSessionKeepsItsWorktree(t *testing.T) {
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
	st1 := startScriptedTurn(t, e, b.ID)
	st1.finish()
	first := sessionInfo(t, e, b.ID)

	st2 := startScriptedTurn(t, e, b.ID)
	st2.finish()
	second := sessionInfo(t, e, b.ID)

	if first.Dir != second.Dir || first.Branch != second.Branch {
		t.Fatalf("an upgraded session must stay put across turns: %+v vs %+v", first, second)
	}
	if list := git(t, repo, "worktree", "list"); strings.Count(list, "\n") != 1 {
		t.Fatalf("a second turn must not grow another worktree:\n%s", list)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
