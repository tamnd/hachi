package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/tamnd/hachi/adapter"
	"github.com/tamnd/hachi/engine"
	"github.com/tamnd/hachi/journal"
	"github.com/tamnd/hachi/waggle"
)

// fakeStream blocks after its first event until stopped, like a long
// agent turn.
type fakeStream struct {
	ch   chan waggle.Event
	stop chan struct{}
}

func (s *fakeStream) Events() <-chan waggle.Event { return s.ch }

func (s *fakeStream) Stop(ctx context.Context) error {
	close(s.stop)
	return nil
}

type fakeAdapter struct{}

func (fakeAdapter) Run(ctx context.Context, sess adapter.Session, msg string) (adapter.Stream, error) {
	s := &fakeStream{ch: make(chan waggle.Event, 8), stop: make(chan struct{})}
	s.ch <- waggle.Event{Bee: "fake", Kind: waggle.KindSpawned, At: time.Now(),
		Data: waggle.Enc(waggle.Spawned{Resume: "thread-1", Brain: "fake"})}
	go func() {
		<-s.stop
		// Simulate wind-down work before the stream actually closes.
		time.Sleep(50 * time.Millisecond)
		close(s.ch)
	}()
	return s, nil
}

func init() {
	adapter.Register(adapter.Info{Name: "fake", Steer: adapter.SteerResume},
		func() (adapter.Adapter, error) { return fakeAdapter{}, nil })
}

// TestStopThenSend pins the steer contract: Stop is synchronous, so a
// Send immediately after must never be rejected as busy.
func TestStopThenSend(t *testing.T) {
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
	if err := e.Send(t.Context(), info.ID, "first"); err != nil {
		t.Fatal(err)
	}
	// The turn is now running and will not finish on its own.
	if err := e.Send(t.Context(), info.ID, "too soon"); err == nil {
		t.Fatal("second Send during a running turn must be rejected")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := e.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := e.Send(t.Context(), info.ID, "steer"); err != nil {
		t.Fatalf("Send right after Stop must succeed, got: %v", err)
	}
	if err := e.Stop(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
}
