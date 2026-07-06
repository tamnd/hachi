package engine_test

// The S2 gate: deliberate mess, restore, tree byte-identical, and it
// works in a non-git folder. Byte-identical is only worth saying if a
// machine checks it, so these tests run on every commit. After restore,
// no observer (git, a hash walk, or the user's own eyes) can tell the
// session ever ran.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/hachi/adapter"
	"github.com/tamnd/hachi/engine"
	"github.com/tamnd/hachi/journal"
	"github.com/tamnd/hachi/waggle"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path string, data []byte, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // WriteFile skips chmod on existing files
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// setupMessRepo builds the pre-baseline state: a committed tree with a
// binary, a symlink, and an executable, deliberately left dirty with an
// unstaged mod, a staged-only file, and an untracked scratch file.
func setupMessRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "test@hachi.local")
	git(t, dir, "config", "user.name", "hachi test")
	git(t, dir, "config", "commit.gpgsign", "false")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "root")

	write(t, filepath.Join(dir, "tracked.txt"), []byte("line1\nline2\n"), 0o644)
	write(t, filepath.Join(dir, "sub", "deep.txt"), []byte("keep\n"), 0o644)
	write(t, filepath.Join(dir, "blob.bin"), []byte{0x00, 0x01, 0x02, 'b', 'i', 'n'}, 0o644)
	write(t, filepath.Join(dir, "script.sh"), []byte("#!/bin/sh\n"), 0o755)
	if err := os.Symlink("tracked.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")

	appendTo(t, filepath.Join(dir, "tracked.txt"), "dirty\n")             // unstaged mod at baseline
	write(t, filepath.Join(dir, "staged.txt"), []byte("staged\n"), 0o644) // staged-only at baseline
	git(t, dir, "add", "staged.txt")
	write(t, filepath.Join(dir, "untracked.txt"), []byte("scratch\n"), 0o644) // untracked at baseline
	return dir
}

// runMess plays the agent: every mutation class in one pass.
func runMess(t *testing.T, dir string) {
	t.Helper()
	appendTo(t, filepath.Join(dir, "tracked.txt"), "agent\n")                // modify tracked
	if err := os.Remove(filepath.Join(dir, "sub", "deep.txt")); err != nil { // delete tracked
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "created.txt"), []byte("new\n"), 0o644) // create file
	appendTo(t, filepath.Join(dir, "blob.bin"), "\x03\x04")             // modify binary
	if err := os.Remove(filepath.Join(dir, "link.txt")); err != nil {   // retarget symlink
		t.Fatal(err)
	}
	if err := os.Symlink("created.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "script.sh"), 0o644); err != nil { // flip exec bit
		t.Fatal(err)
	}
	appendTo(t, filepath.Join(dir, "untracked.txt"), "more\n") // modify untracked-at-baseline
}

func gitStatusZ(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "status", "--porcelain=v1", "-z")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	return string(out)
}

// hashTree records every path outside .git as mode plus content hash, or
// the link target for symlinks; the ground truth git does not look at.
func hashTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out[rel] = "link " + target
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[rel] = fmt.Sprintf("%v %s", info.Mode(), hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func compareHashes(want, got map[string]string) string {
	var lines []string
	for path, w := range want {
		g, ok := got[path]
		switch {
		case !ok:
			lines = append(lines, fmt.Sprintf("missing %s", path))
		case g != w:
			lines = append(lines, fmt.Sprintf("differs %s: want %s, got %s", path, w, g))
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			lines = append(lines, fmt.Sprintf("extra %s", path))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func newEngine(t *testing.T) *engine.Engine {
	t.Helper()
	j, err := journal.NewFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return engine.New(j)
}

func TestRestoreByteIdentical(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := setupMessRepo(t)

	wantStatus := gitStatusZ(t, dir)
	wantHashes := hashTree(t, dir)

	e := newEngine(t)
	info, err := e.Open(t.Context(), "", dir, "fake")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := e.Baseline(t.Context(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.BaselineOID == "" || meta.CleanAtBaseline || meta.UnbornAtBaseline {
		t.Fatalf("dirty tree must snapshot a stash commit, got %+v", meta)
	}
	// Capture must not disturb the tree, the index, or the stash.
	if got := gitStatusZ(t, dir); got != wantStatus {
		t.Fatalf("capture changed git status:\n want %q\n got  %q", wantStatus, got)
	}
	if list := git(t, dir, "stash", "list"); list != "" {
		t.Fatalf("capture touched the stash reflog: %q", list)
	}

	runMess(t, dir)
	rep, err := e.RestoreAll(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(rep.Skipped) != 0 {
		t.Errorf("nothing should be skipped, got %+v", rep.Skipped)
	}

	if got := gitStatusZ(t, dir); got != wantStatus {
		t.Errorf("git status drifted:\n want %q\n got  %q", wantStatus, got)
	}
	if diff := compareHashes(wantHashes, hashTree(t, dir)); diff != "" {
		t.Errorf("tree not byte-identical:\n%s", diff)
	}
}

func TestRestoreByteIdenticalUnbornRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "test@hachi.local")
	git(t, dir, "config", "user.name", "hachi test")
	write(t, filepath.Join(dir, "a.txt"), []byte("staged\n"), 0o644)
	git(t, dir, "add", "a.txt")
	write(t, filepath.Join(dir, "b.txt"), []byte("loose\n"), 0o644)

	wantStatus := gitStatusZ(t, dir)
	wantHashes := hashTree(t, dir)

	e := newEngine(t)
	info, err := e.Open(t.Context(), "", dir, "fake")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := e.Baseline(t.Context(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.UnbornAtBaseline {
		t.Fatalf("no-commit repo must flag UnbornAtBaseline, got %+v", meta)
	}

	appendTo(t, filepath.Join(dir, "a.txt"), "agent\n")
	appendTo(t, filepath.Join(dir, "b.txt"), "agent\n")
	write(t, filepath.Join(dir, "c.txt"), []byte("new\n"), 0o644)

	if _, err := e.RestoreAll(context.Background(), info.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := gitStatusZ(t, dir); got != wantStatus {
		t.Errorf("git status drifted:\n want %q\n got  %q", wantStatus, got)
	}
	if diff := compareHashes(wantHashes, hashTree(t, dir)); diff != "" {
		t.Errorf("tree not byte-identical:\n%s", diff)
	}
}

// scriptStream hands the event channel to the test, so a test can play
// an agent whose edit events and disk writes happen in lockstep, the way
// a real adapter's do.
type scriptStream struct {
	ch chan waggle.Event
}

func (s *scriptStream) Events() <-chan waggle.Event { return s.ch }
func (s *scriptStream) Stop(context.Context) error  { return nil }

var scriptStreams = make(chan *scriptStream, 4)

type scriptedAdapter struct{}

func (scriptedAdapter) Run(ctx context.Context, sess adapter.Session, msg string) (adapter.Stream, error) {
	s := &scriptStream{ch: make(chan waggle.Event, 64)}
	scriptStreams <- s
	return s, nil
}

func init() {
	adapter.Register(adapter.Info{Name: "scripted", Steer: adapter.SteerNone},
		func() (adapter.Adapter, error) { return scriptedAdapter{}, nil })
}

// scriptedTurn drives one turn's edit events and waits for the engine to
// process each before the test mutates disk, mirroring how a real agent
// writes only after announcing the edit.
type scriptedTurn struct {
	t      *testing.T
	stream *scriptStream
	watch  <-chan waggle.Event
}

func startScriptedTurn(t *testing.T, e *engine.Engine, id waggle.SessionID) *scriptedTurn {
	t.Helper()
	watch, err := e.Watch(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Send(t.Context(), id, "make a mess"); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-scriptStreams:
		return &scriptedTurn{t: t, stream: s, watch: watch}
	case <-time.After(5 * time.Second):
		t.Fatal("adapter never ran")
		return nil
	}
}

func (st *scriptedTurn) edit(status, op, path string) {
	st.t.Helper()
	st.stream.ch <- waggle.Event{Bee: "scripted", Kind: waggle.KindEdit, At: time.Now(),
		Data: waggle.Enc(waggle.Edit{Status: status, Changes: []waggle.FileChange{{Path: path, Op: op}}})}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-st.watch:
			if ev.Kind == waggle.KindEdit {
				return // the engine processed the baseline hook before fan-out
			}
		case <-deadline:
			st.t.Fatalf("engine never surfaced the %s %s edit", status, path)
		}
	}
}

func (st *scriptedTurn) finish() {
	close(st.stream.ch)
}

func TestRestoreByteIdenticalNonGit(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "text.txt"), []byte("alpha\nbeta\n"), 0o644)
	write(t, filepath.Join(dir, "bin.bin"), []byte{0x00, 0x01, 0x02}, 0o644)
	write(t, filepath.Join(dir, "sub", "keep.txt"), []byte("keep\n"), 0o644)
	if err := os.Symlink("text.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 8<<20+1) // one byte over the per-file cap
	big[0] = 'B'
	write(t, filepath.Join(dir, "big.bin"), big, 0o644)

	wantHashes := hashTree(t, dir)

	e := newEngine(t)
	info, err := e.Open(t.Context(), "", dir, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	st := startScriptedTurn(t, e, info.ID)

	st.edit("in_progress", "update", "text.txt")
	appendTo(t, filepath.Join(dir, "text.txt"), "agent\n")
	st.edit("completed", "update", "text.txt")

	st.edit("in_progress", "delete", "sub/keep.txt")
	if err := os.Remove(filepath.Join(dir, "sub", "keep.txt")); err != nil {
		t.Fatal(err)
	}
	st.edit("completed", "delete", "sub/keep.txt")

	st.edit("in_progress", "add", "made/new.txt")
	write(t, filepath.Join(dir, "made", "new.txt"), []byte("new\n"), 0o644)
	st.edit("completed", "add", "made/new.txt")

	st.edit("in_progress", "add", "temp.txt") // created, then deleted by the agent
	write(t, filepath.Join(dir, "temp.txt"), []byte("temp\n"), 0o644)
	st.edit("completed", "add", "temp.txt")
	st.edit("in_progress", "delete", "temp.txt")
	if err := os.Remove(filepath.Join(dir, "temp.txt")); err != nil {
		t.Fatal(err)
	}
	st.edit("completed", "delete", "temp.txt")

	st.edit("in_progress", "update", "link.txt")
	if err := os.Remove(filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bin.bin", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	st.edit("completed", "update", "link.txt")

	st.edit("in_progress", "update", "big.bin")
	appendTo(t, filepath.Join(dir, "big.bin"), "x")
	st.edit("completed", "update", "big.bin")
	st.finish()

	rep, err := e.RestoreAll(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	// The over-cap file must be reported as skipped, not just tolerated.
	var bigSkipped bool
	for _, s := range rep.Skipped {
		if s.Path == "big.bin" {
			bigSkipped = true
		}
	}
	if !bigSkipped {
		t.Errorf("big.bin must be reported as skipped, got %+v", rep.Skipped)
	}
	// And the moment it happened, the transcript must have said so.
	events, err := e.Journal.Replay(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	var warned bool
	for _, ev := range events {
		if ev.Kind == waggle.KindFinding && strings.Contains(string(ev.Data), "big.bin") {
			warned = true
		}
	}
	if !warned {
		t.Error("no transcript warning that big.bin has no saved copy")
	}

	got := hashTree(t, dir)
	delete(wantHashes, "big.bin") // undo cannot cover it, by design
	delete(got, "big.bin")
	if diff := compareHashes(wantHashes, got); diff != "" {
		t.Errorf("tree not byte-identical:\n%s", diff)
	}
	if _, err := os.Stat(filepath.Join(dir, "made")); !os.IsNotExist(err) {
		t.Error("directory created for made/new.txt should be pruned after undo")
	}
}

func TestRestoreSkipsOutsideEdits(t *testing.T) {
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

	// The human edits after the agent; their bytes must survive undo.
	write(t, filepath.Join(dir, "text.txt"), []byte("mine\n"), 0o644)

	rep, err := e.RestoreAll(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0].Path != "text.txt" {
		t.Fatalf("expected text.txt skipped, got %+v", rep.Skipped)
	}
	data, err := os.ReadFile(filepath.Join(dir, "text.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "mine\n" {
		t.Errorf("outside edit was overwritten: %q", data)
	}
}
