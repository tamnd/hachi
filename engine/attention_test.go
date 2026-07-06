package engine_test

// Needs-you detection, engine side. Codex runs headless and never
// blocks on stdin, so every raise here is the engine's own reading of
// the turn: a final message ending in a question mark, a clean finish
// with an unaccepted diff, a death. Raises anchor on the result event;
// an interrupted stream never sends one and never raises anything.

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/tamnd/hachi/engine"
	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

// event pushes one scripted event without waiting for fan-out; finish()
// drains the pump before returning, so ordering still holds.
func (st *scriptedTurn) event(kind waggle.Kind, data []byte) {
	st.stream.ch <- waggle.Event{Bee: "scripted", Kind: kind, At: time.Now(), Data: data}
}

func (st *scriptedTurn) say(text string) {
	st.event(waggle.KindMessage, waggle.Enc(waggle.Message{Text: text}))
}

func (st *scriptedTurn) result() { st.event(waggle.KindResult, nil) }

func (st *scriptedTurn) die(msg string) {
	st.event(waggle.KindDied, waggle.Enc(waggle.Died{Error: msg}))
}

func listed(t *testing.T, e *engine.Engine, id waggle.SessionID) hive.SessionInfo {
	t.Helper()
	list, err := e.Sessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("session %s missing from the list", id)
	return hive.SessionInfo{}
}

func replay(t *testing.T, e *engine.Engine, id waggle.SessionID) []waggle.Event {
	t.Helper()
	evs, err := e.Journal.Replay(id)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestQuestionRaisesUntilAnswered(t *testing.T) {
	repo := newRepo(t)
	e := newEngine(t)
	info, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}

	st := startScriptedTurn(t, e, info.ID)
	st.say("I can do this two ways.\n\nShould I also update the v1 endpoints?")
	st.result()
	st.finish()

	s := listed(t, e, info.ID)
	if s.State != hive.StateNeeds || s.Reason != "question" {
		t.Fatalf("a closing question must read as needs/question, got %s/%s", s.State, s.Reason)
	}
	if s.Detail != "Should I also update the v1 endpoints?" {
		t.Fatalf("the detail is the question itself, got %q", s.Detail)
	}
	if got := sessionInfo(t, e, info.ID); got.Reason != "question" {
		t.Fatal("Open must report the same raise the list does")
	}

	// The ask is journaled as an engine-origin need_input, so a replay
	// can re-raise it.
	var ni waggle.NeedInput
	found := false
	for _, ev := range replay(t, e, info.ID) {
		if ev.Kind == waggle.KindNeedInput {
			if err := json.Unmarshal(ev.Data, &ni); err != nil {
				t.Fatal(err)
			}
			found = true
		}
	}
	if !found || ni.Origin != "engine" || ni.Prompt != "Should I also update the v1 endpoints?" {
		t.Fatalf("journal must carry the engine-origin ask, got %+v found=%v", ni, found)
	}

	// Looking is not answering: Seen leaves a question raised.
	if err := e.Seen(t.Context(), info.ID); err != nil {
		t.Fatal(err)
	}
	if s := listed(t, e, info.ID); s.Reason != "question" {
		t.Fatal("Seen must not clear a question")
	}

	// Answering clears it: the next turn starting is the reply.
	st = startScriptedTurn(t, e, info.ID)
	if s := listed(t, e, info.ID); s.State != hive.StateWorking || s.Reason != "" {
		t.Fatalf("a running answer clears the ask, got %s/%s", s.State, s.Reason)
	}
	st.finish()
}

func TestFreshDiffRaisesAndSeenParksIt(t *testing.T) {
	repo := newRepo(t)
	e := newEngine(t)
	info, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}

	st := startScriptedTurn(t, e, info.ID)
	st.edit("in_progress", "create", "notes.txt")
	write(t, filepath.Join(repo, "notes.txt"), []byte("draft\n"), 0o644)
	st.edit("completed", "create", "notes.txt")
	st.say("Done, notes.txt has the draft.")
	st.result()
	st.finish()

	s := listed(t, e, info.ID)
	if s.State != hive.StateNeeds || s.Reason != "diff" {
		t.Fatalf("a clean finish with unaccepted changes must read needs/diff, got %s/%s", s.State, s.Reason)
	}
	if !s.DiffReady {
		t.Fatal("DiffReady rides along with the raise")
	}

	// Opening the diff parks it back in review: still DiffReady, no
	// longer needing you, and the look is journaled.
	if err := e.Seen(t.Context(), info.ID); err != nil {
		t.Fatal(err)
	}
	s = listed(t, e, info.ID)
	if s.State != hive.StateIdle || s.Reason != "" || !s.DiffReady {
		t.Fatalf("seen parks the diff in review, got %s/%s ready=%v", s.State, s.Reason, s.DiffReady)
	}
	seen := false
	for _, ev := range replay(t, e, info.ID) {
		var mk waggle.Marker
		if ev.Kind == waggle.KindMarker && json.Unmarshal(ev.Data, &mk) == nil && mk.Name == "seen" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("the look must land in the journal as a seen marker")
	}

	// Accepting the changes clears the parked state too.
	if _, err := e.Stage(t.Context(), info.ID, nil); err != nil {
		t.Fatal(err)
	}
	if s := listed(t, e, info.ID); s.DiffReady {
		t.Fatal("staged changes leave nothing to review")
	}
}

func TestDiedRaisesUntilSeen(t *testing.T) {
	repo := newRepo(t)
	e := newEngine(t)
	info, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}

	st := startScriptedTurn(t, e, info.ID)
	st.die("exit status 1: codex exploded")
	st.finish()

	s := listed(t, e, info.ID)
	if s.State != hive.StateDied || s.Reason != "died" {
		t.Fatalf("a death must read died/died, got %s/%s", s.State, s.Reason)
	}
	if s.Detail != "exit status 1: codex exploded" {
		t.Fatalf("the detail is the death's own error, got %q", s.Detail)
	}

	// First open after the death acknowledges it; the session reads
	// idle again and the board files it under done.
	if err := e.Seen(t.Context(), info.ID); err != nil {
		t.Fatal(err)
	}
	if s := listed(t, e, info.ID); s.State != hive.StateIdle || s.Reason != "" {
		t.Fatalf("an acknowledged death reads idle, got %s/%s", s.State, s.Reason)
	}
}

func TestInterruptedTurnRaisesNothing(t *testing.T) {
	repo := newRepo(t)
	e := newEngine(t)
	info, err := e.Open(t.Context(), "", repo, "scripted")
	if err != nil {
		t.Fatal(err)
	}

	// A question and a dirty tree, but no result event: the stream was
	// cut, which means the human did the cutting and is already here.
	st := startScriptedTurn(t, e, info.ID)
	st.edit("in_progress", "create", "notes.txt")
	write(t, filepath.Join(repo, "notes.txt"), []byte("draft\n"), 0o644)
	st.edit("completed", "create", "notes.txt")
	st.say("Should I keep going?")
	st.finish()

	s := listed(t, e, info.ID)
	if s.State != hive.StateIdle || s.Reason != "" {
		t.Fatalf("an interrupted turn raises nothing, got %s/%s", s.State, s.Reason)
	}
	if !s.DiffReady {
		t.Fatal("the unaccepted changes still count as a diff in review")
	}
}
