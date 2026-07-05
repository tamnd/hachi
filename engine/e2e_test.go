package engine_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	_ "github.com/tamnd/hachi/adapter/codex"
	"github.com/tamnd/hachi/engine"
	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/journal"
	"github.com/tamnd/hachi/waggle"
)

// TestRealCodexTurn drives a real codex binary with the real account.
// No mocks: this is the S0 gate. It skips when codex is not installed
// or when -short is set.
func TestRealCodexTurn(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not installed")
	}

	j, err := journal.NewFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	e := engine.New(j)

	info, err := e.Open(context.Background(), "", t.TempDir(), "codex")
	if err != nil {
		t.Fatal(err)
	}

	turn(t, e, info.ID, "Reply with exactly the word HACHI_OK and nothing else.", "HACHI_OK")

	// Second turn must resume the same thread: the model can only answer
	// this if the first turn's context survived.
	turn(t, e, info.ID, "Repeat the exact word I asked for in my previous message, nothing else.", "HACHI_OK")
}

func turn(t *testing.T, e *engine.Engine, id waggle.SessionID, prompt, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ch, err := e.Watch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Send(ctx, id, prompt); err != nil {
		t.Fatal(err)
	}

	var gotMsg, gotCost, gotResult bool
	for ev := range ch {
		switch ev.Kind {
		case waggle.KindMessage:
			if ev.Bee == "human" {
				continue
			}
			var m waggle.Message
			if err := json.Unmarshal(ev.Data, &m); err != nil {
				t.Fatalf("bad message payload: %v", err)
			}
			if strings.Contains(m.Text, want) {
				gotMsg = true
			}
		case waggle.KindCost:
			gotCost = true
		case waggle.KindDied:
			var d waggle.Died
			_ = json.Unmarshal(ev.Data, &d)
			t.Fatalf("turn died: %s", d.Error)
		case waggle.KindResult:
			gotResult = true
		}
		if gotResult {
			break
		}
	}
	if !gotMsg {
		t.Fatalf("no agent message containing %q", want)
	}
	if !gotCost {
		t.Fatal("no cost event")
	}

	// The turn must settle back to idle so the next Send is accepted.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if state(t, e, id) != hive.StateWorking {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("session stuck in working state after result")
}

func state(t *testing.T, e *engine.Engine, id waggle.SessionID) hive.State {
	t.Helper()
	list, err := e.Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.ID == id {
			return s.State
		}
	}
	t.Fatalf("session %s missing from list", id)
	return hive.StateIdle
}
