package engine_test

// S4 concurrency property: three sessions run turns at once through one
// engine, and every session's watcher sees its own events, in order,
// gap-free, no matter how the streams interleave. This is the invariant
// the board and the attention strip stand on.

import (
	"sync"
	"testing"
	"time"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

func TestParallelTurnsKeepPerSessionOrder(t *testing.T) {
	const turns = 3
	const events = 200

	e := newEngine(t)
	sts := make([]*scriptedTurn, turns)
	for i := range sts {
		info, err := e.Open(t.Context(), "", t.TempDir(), "scripted")
		if err != nil {
			t.Fatal(err)
		}
		sts[i] = startScriptedTurn(t, e, info.ID)
	}

	// All three must be working at once; parallel queens, not a queue.
	list, err := e.Sessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	working := 0
	for _, s := range list {
		if s.State == hive.StateWorking {
			working++
		}
	}
	if working != turns {
		t.Fatalf("want %d sessions working at once, got %d", turns, working)
	}

	// Consumers first, so the fan-out buffers never fill and drop.
	var wg sync.WaitGroup
	for _, st := range sts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Seq 1 is the human message already replayed by Watch.
			last := uint64(0)
			deadline := time.After(30 * time.Second)
			for seen := 0; seen < events+1; seen++ {
				select {
				case ev := <-st.watch:
					if ev.Sess != st.id {
						t.Errorf("session %s got an event for %s", st.id, ev.Sess)
						return
					}
					if ev.Seq != last+1 {
						t.Errorf("session %s: seq %d after %d, order or a gap broke", st.id, ev.Seq, last)
						return
					}
					last = ev.Seq
				case <-deadline:
					t.Errorf("session %s: only %d of %d events arrived", st.id, seen, events+1)
					return
				}
			}
		}()
	}

	for _, st := range sts {
		go func() {
			for i := 0; i < events; i++ {
				st.stream.ch <- waggle.Event{Bee: "scripted", Kind: waggle.KindMessage, At: time.Now(),
					Data: waggle.Enc(waggle.Message{Text: "tick"})}
			}
		}()
	}

	wg.Wait()
	for _, st := range sts {
		st.finish()
	}
}
