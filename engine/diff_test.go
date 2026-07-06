package engine_test

// Diff-so-far runs against the same deliberate mess the restore tests
// use, so what the human reads on the diff screen and what undo puts
// back can never drift apart.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tamnd/hachi/hive"
)

func byPath(diffs []hive.FileDiff) map[string]hive.FileDiff {
	out := map[string]hive.FileDiff{}
	for _, d := range diffs {
		out[d.Path] = d
	}
	return out
}

func TestChangesGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
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

	diffs, err := e.Changes(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := byPath(diffs)

	tr, ok := got["tracked.txt"]
	if !ok || tr.Status != "M" {
		t.Fatalf("tracked.txt must be M, got %+v", got["tracked.txt"])
	}
	// The baseline already held the dirty line; only the agent's line may
	// show as an addition.
	if !strings.Contains(tr.Patch, "+agent") {
		t.Errorf("tracked.txt patch misses the agent's line:\n%s", tr.Patch)
	}
	if strings.Contains(tr.Patch, "+dirty") || strings.Contains(tr.Patch, "-dirty") {
		t.Errorf("tracked.txt patch blames pre-session dirt on the agent:\n%s", tr.Patch)
	}

	if d, ok := got["sub/deep.txt"]; !ok || d.Status != "D" || !strings.Contains(d.Patch, "-keep") {
		t.Errorf("sub/deep.txt must be D with the removed line, got %+v", got["sub/deep.txt"])
	}
	if c, ok := got["created.txt"]; !ok || c.Status != "A" || !strings.Contains(c.Patch, "+new") {
		t.Errorf("created.txt must be A with its content, got %+v", got["created.txt"])
	}
	if b, ok := got["blob.bin"]; !ok || !b.Binary || !strings.Contains(b.Note, "binary file changed (6 B → 8 B)") {
		t.Errorf("blob.bin must be binary with sizes, got %+v", got["blob.bin"])
	}
	if s, ok := got["script.sh"]; !ok || !strings.Contains(s.Note, "100755 → 100644") {
		t.Errorf("script.sh must report the mode flip, got %+v", got["script.sh"])
	}
	if l, ok := got["link.txt"]; !ok || l.Status != "M" || !strings.Contains(l.Patch, "+created.txt") {
		t.Errorf("link.txt must show the retarget, got %+v", got["link.txt"])
	}
	if u, ok := got["untracked.txt"]; !ok || u.Status != "M" || !strings.Contains(u.Patch, "+more") {
		t.Errorf("untracked.txt must diff against its baseline copy, got %+v", got["untracked.txt"])
	}
	// staged.txt sat staged at baseline and was never touched: silence.
	if s, ok := got["staged.txt"]; ok {
		t.Errorf("untouched staged.txt must not appear, got %+v", s)
	}
	if len(diffs) != 7 {
		t.Errorf("want 7 changed files, got %d: %+v", len(diffs), diffs)
	}
}

func TestChangesGitCleanBeforeMess(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := setupMessRepo(t)
	e := newEngine(t)
	info, err := e.Open(t.Context(), "", dir, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Baseline(t.Context(), info.ID); err != nil {
		t.Fatal(err)
	}
	diffs, err := e.Changes(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Errorf("nothing changed yet, got %+v", diffs)
	}
}

func TestChangesNonGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH") // patches render through git diff --no-index
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "text.txt"), []byte("alpha\nbeta\n"), 0o644)
	write(t, filepath.Join(dir, "gone.txt"), []byte("bye\n"), 0o644)

	e := newEngine(t)
	info, err := e.Open(t.Context(), "", dir, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	st := startScriptedTurn(t, e, info.ID)

	st.edit("in_progress", "update", "text.txt")
	appendTo(t, filepath.Join(dir, "text.txt"), "agent\n")
	st.edit("completed", "update", "text.txt")

	st.edit("in_progress", "delete", "gone.txt")
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	st.edit("completed", "delete", "gone.txt")

	st.edit("in_progress", "add", "made/new.txt")
	write(t, filepath.Join(dir, "made", "new.txt"), []byte("new\n"), 0o644)
	st.edit("completed", "add", "made/new.txt")
	st.finish()

	diffs, err := e.Changes(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := byPath(diffs)
	if d, ok := got["text.txt"]; !ok || d.Status != "M" || !strings.Contains(d.Patch, "+agent") {
		t.Errorf("text.txt must be M with the agent's line, got %+v", got["text.txt"])
	}
	if d, ok := got["gone.txt"]; !ok || d.Status != "D" || !strings.Contains(d.Patch, "-bye") {
		t.Errorf("gone.txt must be D with the lost line, got %+v", got["gone.txt"])
	}
	if d, ok := got["made/new.txt"]; !ok || d.Status != "A" || !strings.Contains(d.Patch, "+new") {
		t.Errorf("made/new.txt must be A, got %+v", got["made/new.txt"])
	}
	if len(diffs) != 3 {
		t.Errorf("want 3 changed files, got %d: %+v", len(diffs), diffs)
	}
}

func TestChangesFlagsOutsideEdit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
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
	st.finish()

	// The human piles on after the agent.
	write(t, filepath.Join(dir, "text.txt"), []byte("mine\n"), 0o644)

	diffs, err := e.Changes(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := byPath(diffs)
	d, ok := got["text.txt"]
	if !ok || !d.Outside {
		t.Fatalf("text.txt must carry the outside-edit flag, got %+v", got["text.txt"])
	}
	if !strings.Contains(d.Patch, "+mine") {
		t.Errorf("the diff still shows the file's current state:\n%s", d.Patch)
	}
}
