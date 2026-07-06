package engine_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tamnd/hachi/adapter"
	"github.com/tamnd/hachi/engine"
	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/journal"
	"github.com/tamnd/hachi/waggle"
)

// quickAdapter completes a one-event turn on its own, unlike the blocking
// fake in stop_test.
type quickAdapter struct{}

type quickStream struct{ ch chan waggle.Event }

func (s *quickStream) Events() <-chan waggle.Event    { return s.ch }
func (s *quickStream) Stop(ctx context.Context) error { return nil }

func (quickAdapter) Run(ctx context.Context, sess adapter.Session, msg string) (adapter.Stream, error) {
	s := &quickStream{ch: make(chan waggle.Event, 8)}
	s.ch <- waggle.Event{Bee: "quick", Kind: waggle.KindMessage, At: time.Now(),
		Data: waggle.Enc(waggle.Message{Text: "done: " + msg})}
	s.ch <- waggle.Event{Bee: "quick", Kind: waggle.KindResult, At: time.Now()}
	close(s.ch)
	return s, nil
}

func init() {
	adapter.Register(adapter.Info{Name: "quick", Steer: adapter.SteerResume},
		func() (adapter.Adapter, error) { return quickAdapter{}, nil })
}

// TestReopenContinuesSequence pins the crash-and-reopen contract: a fresh
// process must continue the journal's sequence numbers, not restart at
// one. A restart would collide with replayed events and clients that
// deduplicate on seq would silently drop the whole new turn.
func TestReopenContinuesSequence(t *testing.T) {
	dir := t.TempDir()
	work := t.TempDir()

	j1, err := journal.NewFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	e1 := engine.New(j1)
	info, err := e1.Open(t.Context(), "", work, "quick")
	if err != nil {
		t.Fatal(err)
	}
	if err := e1.Send(t.Context(), info.ID, "first turn"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e1, info.ID)
	_ = j1.Close()

	// A new engine over the same journal is a process restart.
	j2, err := journal.NewFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j2.Close() }()
	e2 := engine.New(j2)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := e2.Watch(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e2.Send(t.Context(), info.ID, "second turn"); err != nil {
		t.Fatal(err)
	}

	var lastSeq uint64
	var sawSecond bool
	deadline := time.After(10 * time.Second)
	for !sawSecond {
		select {
		case ev := <-ch:
			if ev.Seq <= lastSeq {
				t.Fatalf("sequence went backwards after reopen: %d then %d (kind %s)", lastSeq, ev.Seq, ev.Kind)
			}
			lastSeq = ev.Seq
			var msg waggle.Message
			if ev.Kind == waggle.KindMessage && ev.Bee == "quick" &&
				ev.Data != nil && json.Unmarshal(ev.Data, &msg) == nil &&
				msg.Text == "done: second turn" {
				sawSecond = true
			}
		case <-deadline:
			t.Fatal("never saw the second turn's reply with a fresh sequence number")
		}
	}
	// The reply fans out before the pump's final meta save; wait the turn
	// out so nothing races the TempDir cleanup.
	waitIdle(t, e2, info.ID)
}

func waitIdle(t *testing.T, e *engine.Engine, id waggle.SessionID) {
	t.Helper()
	for range 100 {
		list, err := e.Sessions(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.ID == id && s.State != hive.StateWorking {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("turn never finished")
}
