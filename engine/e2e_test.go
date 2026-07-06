package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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

// TestRealCodexToolUse makes the agent actually run a command and write a
// file, then checks that tool and edit events came through with refs and
// that the file really exists. This is the S1 gate for rich blocks: the
// cards in the TUI are only as real as these events.
func TestRealCodexToolUse(t *testing.T) {
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

	dir := t.TempDir()
	info, err := e.Open(context.Background(), "", dir, "codex")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ch, err := e.Watch(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Send(ctx, info.ID,
		"Create a file named proof.txt containing exactly the line HACHI_TOOL_OK using a shell command. Then reply DONE."); err != nil {
		t.Fatal(err)
	}

	var completedTools, completedEdits int
	for ev := range ch {
		switch ev.Kind {
		case waggle.KindTool:
			var tool waggle.Tool
			if err := json.Unmarshal(ev.Data, &tool); err != nil {
				t.Fatalf("bad tool payload: %v", err)
			}
			if tool.Ref == "" {
				t.Errorf("tool event without ref: %+v", tool)
			}
			if tool.Status == "completed" {
				completedTools++
			}
		case waggle.KindEdit:
			var ed waggle.Edit
			if err := json.Unmarshal(ev.Data, &ed); err != nil {
				t.Fatalf("bad edit payload: %v", err)
			}
			if ed.Ref == "" {
				t.Errorf("edit event without ref: %+v", ed)
			}
			if ed.Status == "completed" {
				completedEdits++
			}
		case waggle.KindDied:
			var d waggle.Died
			_ = json.Unmarshal(ev.Data, &d)
			t.Fatalf("turn died: %s", d.Error)
		case waggle.KindResult:
			goto done
		}
	}
done:
	drain(t, e, info.ID)
	if completedTools+completedEdits == 0 {
		t.Fatal("agent produced no completed tool or edit events")
	}
	b, err := os.ReadFile(filepath.Join(dir, "proof.txt"))
	if err != nil {
		t.Fatalf("agent claimed to write proof.txt but: %v", err)
	}
	if !strings.Contains(string(b), "HACHI_TOOL_OK") {
		t.Fatalf("proof.txt has wrong content: %q", b)
	}
}

// TestRealCodexSteer stops a long-running turn mid-flight and immediately
// sends a new message on the same session: the esc-then-type flow. Stop is
// synchronous in the engine, so the follow-up Send must never be rejected
// as busy, and the session must still answer with its thread context.
func TestRealCodexSteer(t *testing.T) {
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

	// Seed the thread so the steer turn has context to prove resume.
	turn(t, e, info.ID, "Remember the word HACHI_STEER. Reply OK and nothing else.", "OK")

	// Start a turn that would run for a while, then stop it early.
	ctx := context.Background()
	if err := e.Send(ctx, info.ID,
		"Count from 1 to 50, running a separate shell command echo for each number."); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second) // let it get going before pulling the cord

	stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := e.Stop(stopCtx, info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The steer: an immediate Send must be accepted and the thread must
	// still hold the seeded word.
	turn(t, e, info.ID, "Stop counting. What word did I ask you to remember? Reply with just that word.", "HACHI_STEER")
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

	// Watch replays the whole session first. Skip everything until our own
	// prompt goes by; otherwise a previous turn's result event ends the
	// loop before the live turn even starts, and its messages could
	// satisfy the assertions without exercising resume at all.
	var started, gotMsg, gotCost, gotResult bool
	var replies []string
	for ev := range ch {
		if !started {
			if ev.Kind == waggle.KindMessage && ev.Bee == "human" {
				var m waggle.Message
				_ = json.Unmarshal(ev.Data, &m)
				started = m.Text == prompt
			}
			continue
		}
		switch ev.Kind {
		case waggle.KindMessage:
			if ev.Bee == "human" {
				continue
			}
			var m waggle.Message
			if err := json.Unmarshal(ev.Data, &m); err != nil {
				t.Fatalf("bad message payload: %v", err)
			}
			replies = append(replies, m.Text)
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
		t.Fatalf("no agent message containing %q; agent said: %q", want, replies)
	}
	if !gotCost {
		t.Fatal("no cost event")
	}

	// The turn must fully wind down so the next Send is accepted and no
	// end-of-turn write races the test's teardown.
	drain(t, e, id)
	if state(t, e, id) == hive.StateWorking {
		t.Fatal("session stuck in working state after result")
	}
}

// drain waits out the pump's end-of-turn writes. Stop keeps the turn
// registered until the tail has landed, so it doubles as the barrier;
// on a turn that already finished it returns at once.
func drain(t *testing.T, e *engine.Engine, id waggle.SessionID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.Stop(ctx, id); err != nil {
		t.Fatalf("drain: %v", err)
	}
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
