package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/tamnd/hachi/engine"
	"github.com/tamnd/hachi/journal"
)

// TestResumeSavedMidTurn pins a cost contract: the resume handle hits the
// disk as soon as the brain reports it, not at turn end. If the process
// dies mid-turn the next Send must still resume the same thread; a fresh
// thread would re-bill the whole conversation as uncached input.
func TestResumeSavedMidTurn(t *testing.T) {
	j, err := journal.NewFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	e := engine.New(j)

	info, err := e.Open(t.Context(), "", t.TempDir(), "fake")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Send(t.Context(), info.ID, "go"); err != nil {
		t.Fatal(err)
	}

	// The fake adapter emits its resume handle and then blocks until
	// stopped, so anything we read now is mid-turn state.
	deadline := time.Now().Add(5 * time.Second)
	for {
		m, err := j.LoadMeta(info.ID)
		if err == nil && m.Resume == "thread-1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resume handle not persisted while the turn was still running")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := e.Stop(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
}
