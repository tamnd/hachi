package engine_test

// The non-git wait. A plain folder cannot grow a worktree, so the second
// writer is refused, parked with Queue, and started the moment the first
// session leaves Working with nothing unreviewed. The queued turn must
// be a real turn: adapter run, human message in the journal, the works.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

func TestQueueStartsWhenFolderFrees(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "notes.txt"), []byte("start\n"), 0o644)
	e := newEngine(t)

	a, err := e.Open(t.Context(), "", dir, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stA := startScriptedTurn(t, e, a.ID)

	b, err := e.Open(t.Context(), "", dir, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	watchB, err := e.Watch(t.Context(), b.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The second writer is refused, and the refusal names who has the
	// folder so the client can say it in one sentence.
	err = e.Send(t.Context(), b.ID, "tidy the notes")
	busy, ok := errors.AsType[*hive.FolderBusyError](err)
	if !ok {
		t.Fatalf("a second writer in a busy non-git folder must get a FolderBusyError, got %v", err)
	}
	if busy.With != "make a mess" {
		t.Fatalf("the error must carry the other session's title, got %q", busy.With)
	}

	if err := e.Queue(t.Context(), b.ID, "tidy the notes"); err != nil {
		t.Fatal(err)
	}
	if st := sessionInfo(t, e, b.ID).State; st != hive.StateWaiting {
		t.Fatalf("a queued session must show waiting, got %s", st)
	}

	// A finishes with nothing to review; that is the starting gun.
	stA.finish()

	var streamB *scriptStream
	select {
	case streamB = <-scriptStreams:
	case <-time.After(5 * time.Second):
		t.Fatal("the queued turn never started after the folder freed")
	}
	defer func() {
		close(streamB.ch)
		_ = e.Stop(t.Context(), b.ID)
	}()

	// The queued message becomes a real human message on B's journal.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-watchB:
			if ev.Kind == waggle.KindMessage && ev.Bee == "human" && strings.Contains(string(ev.Data), "tidy the notes") {
				return
			}
		case <-deadline:
			t.Fatal("the parked message never reached B's journal")
		}
	}
}

func TestQueueWaitsOutUnreviewedChanges(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "notes.txt"), []byte("start\n"), 0o644)
	e := newEngine(t)

	a, err := e.Open(t.Context(), "", dir, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stA := startScriptedTurn(t, e, a.ID)
	stA.edit("in_progress", "update", "notes.txt")
	appendTo(t, filepath.Join(dir, "notes.txt"), "mess\n")
	stA.edit("completed", "update", "notes.txt")
	stA.finish()

	b, err := e.Open(t.Context(), "", dir, "scripted")
	if err != nil {
		t.Fatal(err)
	}

	// A is idle but its changes are unreviewed; the folder stays claimed.
	err = e.Send(t.Context(), b.ID, "tidy up after")
	if _, ok := errors.AsType[*hive.FolderBusyError](err); !ok {
		t.Fatalf("unreviewed changes must keep the folder busy, got %v", err)
	}
	if err := e.Queue(t.Context(), b.ID, "tidy up after"); err != nil {
		t.Fatal(err)
	}

	// The dispatch that Queue kicks off must bounce and re-park.
	select {
	case s := <-scriptStreams:
		close(s.ch)
		t.Fatal("the queued turn must not start while changes sit unreviewed")
	case <-time.After(300 * time.Millisecond):
	}
	if st := sessionInfo(t, e, b.ID).State; st != hive.StateWaiting {
		t.Fatalf("the bounced message must go back to waiting, got %s", st)
	}

	// Accepting A's changes clears the claim and releases the queue.
	if _, err := e.Stage(t.Context(), a.ID, nil); err != nil {
		t.Fatal(err)
	}
	var streamB *scriptStream
	select {
	case streamB = <-scriptStreams:
	case <-time.After(5 * time.Second):
		t.Fatal("accepting the changes never released the queued turn")
	}
	close(streamB.ch)
	if err := e.Stop(t.Context(), b.ID); err != nil {
		t.Fatal(err)
	}
}

func TestQueueRefusesRunningSession(t *testing.T) {
	dir := t.TempDir()
	e := newEngine(t)
	a, err := e.Open(t.Context(), "", dir, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stA := startScriptedTurn(t, e, a.ID)
	defer stA.finish()
	if err := e.Queue(t.Context(), a.ID, "again"); err == nil {
		t.Fatal("a session mid-turn has nothing to park; expected an error")
	}
}

func TestSendAllowsSeparateNonGitFolders(t *testing.T) {
	e := newEngine(t)

	a, err := e.Open(t.Context(), "", t.TempDir(), "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stA := startScriptedTurn(t, e, a.ID)
	defer stA.finish()

	// A sibling folder is not a collision; only overlap serializes.
	b, err := e.Open(t.Context(), "", t.TempDir(), "scripted")
	if err != nil {
		t.Fatal(err)
	}
	stB := startScriptedTurn(t, e, b.ID)
	stB.finish()
}
