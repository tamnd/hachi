package engine_test

// DiffReady's lifecycle: it comes up when a turn ends with changes
// nobody accepted, and goes down the moment they are staged or undone.
// The engine caches the flag and refreshes it only at those moments,
// so Sessions stays cheap enough for a client to poll.

import (
	"path/filepath"
	"testing"

	"github.com/tamnd/hachi/engine"
	"github.com/tamnd/hachi/waggle"
)

func diffReady(t *testing.T, e *engine.Engine, id waggle.SessionID) bool {
	t.Helper()
	list, err := e.Sessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.ID == id {
			return s.DiffReady
		}
	}
	t.Fatalf("session %s missing from the list", id)
	return false
}

func TestDiffReadyUpAtTurnEndDownAtStage(t *testing.T) {
	repo := newRepo(t)
	e := newEngine(t)
	info, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}
	if diffReady(t, e, info.ID) {
		t.Fatal("a fresh session has nothing to review")
	}

	st := startScriptedTurn(t, e, info.ID)
	st.edit("in_progress", "create", "notes.txt")
	write(t, filepath.Join(repo, "notes.txt"), []byte("draft\n"), 0o644)
	st.edit("completed", "create", "notes.txt")
	st.finish()

	if !diffReady(t, e, info.ID) {
		t.Fatal("turn ended with an unaccepted change; DiffReady must be up")
	}
	if !sessionInfo(t, e, info.ID).DiffReady {
		t.Fatal("Open must report the same DiffReady the list does")
	}

	if _, err := e.Stage(t.Context(), info.ID, nil); err != nil {
		t.Fatal(err)
	}
	if diffReady(t, e, info.ID) {
		t.Fatal("everything is staged; DiffReady must clear")
	}
}

func TestDiffReadyDownAfterRestore(t *testing.T) {
	repo := newRepo(t)
	e := newEngine(t)
	info, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}

	st := startScriptedTurn(t, e, info.ID)
	st.edit("in_progress", "update", "README.md")
	appendTo(t, filepath.Join(repo, "README.md"), "agent\n")
	st.edit("completed", "update", "README.md")
	st.finish()

	if !diffReady(t, e, info.ID) {
		t.Fatal("turn ended with an unaccepted change; DiffReady must be up")
	}
	if _, err := e.Restore(t.Context(), info.ID, nil); err != nil {
		t.Fatal(err)
	}
	if diffReady(t, e, info.ID) {
		t.Fatal("the change was undone; DiffReady must clear")
	}
}
