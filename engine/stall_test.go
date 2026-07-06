package engine

// White-box on purpose: a stall is silence, and real silence takes real
// minutes. These tests move the clock by writing the engine's own
// last-event stamps instead of sleeping, which only works from inside
// the package. The scripted end-to-end paths live in attention_test.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/journal"
	"github.com/tamnd/hachi/waggle"
)

func stallEngine(t *testing.T) *Engine {
	t.Helper()
	j, err := journal.NewFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(j)
}

// working fakes a codex session mid-turn that has been silent for ten
// minutes, well past the cold-start 4m N. Tests that need a different
// silence move e.last themselves.
func working(t *testing.T, e *Engine, id waggle.SessionID) {
	t.Helper()
	if err := e.Journal.SaveMeta(journal.Meta{ID: id, Dir: t.TempDir(), Brain: "codex", Created: time.Now(), Updated: time.Now()}); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.state[id] = hive.StateWorking
	e.running[id] = &turn{done: make(chan struct{})}
	e.last[id] = time.Now().Add(-10 * time.Minute)
	e.mu.Unlock()
}

func find(t *testing.T, e *Engine, id waggle.SessionID) hive.SessionInfo {
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
	t.Fatalf("session %s missing", id)
	return hive.SessionInfo{}
}

func TestStallRaisesOnThePoll(t *testing.T) {
	e := stallEngine(t)
	working(t, e, "quiet")

	s := find(t, e, "quiet")
	if s.State != hive.StateNeeds || s.Reason != "stall" {
		t.Fatalf("ten silent minutes on a cold-start 4m N must raise, got %s/%s", s.State, s.Reason)
	}
	if s.Detail != "quiet for 10m" {
		t.Fatalf("the detail says how long, got %q", s.Detail)
	}

	// The silence keeps growing while raised; the detail follows.
	e.mu.Lock()
	e.last["quiet"] = time.Now().Add(-25 * time.Minute)
	e.mu.Unlock()
	if s := find(t, e, "quiet"); s.Detail != "quiet for 25m" {
		t.Fatalf("a raised stall's detail must track the clock, got %q", s.Detail)
	}
}

func TestStallClockOnlyRunsMidTurn(t *testing.T) {
	e := stallEngine(t)
	working(t, e, "s")

	// Blocked on the human: the wait is the human's, never paged.
	e.mu.Lock()
	e.state["s"] = hive.StateNeeds
	e.attn["s"] = &attention{reason: "question", detail: "which one?", raised: time.Now()}
	e.mu.Unlock()
	if s := find(t, e, "s"); s.Reason != "question" {
		t.Fatalf("a mid-turn ask pauses the stall clock, got %s", s.Reason)
	}

	// Idle with an old stamp: no turn, no clock.
	e.mu.Lock()
	e.state["s"] = hive.StateIdle
	delete(e.attn, "s")
	delete(e.running, "s")
	e.mu.Unlock()
	if s := find(t, e, "s"); s.Reason != "" {
		t.Fatalf("no running turn means no stall, got %s", s.Reason)
	}
}

func TestBeatHealsAndJournalsNothingItself(t *testing.T) {
	e := stallEngine(t)
	working(t, e, "s")
	find(t, e, "s") // raise it

	if !e.beat("s", "codex") {
		t.Fatal("an event after a raise must heal it")
	}
	if e.beat("s", "codex") {
		t.Fatal("a second beat has nothing to heal")
	}
	if s := find(t, e, "s"); s.Reason != "" {
		t.Fatalf("healed means gone, got %s", s.Reason)
	}
}

func TestKeepWaitingDoublesThisTurn(t *testing.T) {
	e := stallEngine(t)
	working(t, e, "s")
	find(t, e, "s") // raise it

	if err := e.KeepWaiting(t.Context(), "s"); err != nil {
		t.Fatal(err)
	}
	if s := find(t, e, "s"); s.Reason != "" {
		t.Fatalf("w clears the raise, got %s", s.Reason)
	}

	// The press is journaled for the false-positive stats.
	evs, err := e.Journal.Replay("s")
	if err != nil {
		t.Fatal(err)
	}
	waited := false
	for _, ev := range evs {
		var mk waggle.Marker
		if ev.Kind == waggle.KindMarker && json.Unmarshal(ev.Data, &mk) == nil && mk.Name == "stall_wait" {
			waited = true
		}
	}
	if !waited {
		t.Fatal("keep-waiting must land in the journal as a stall_wait marker")
	}

	// Doubled: 6 quiet minutes clears an 8m threshold, 10 does not.
	e.mu.Lock()
	e.last["s"] = time.Now().Add(-6 * time.Minute)
	e.mu.Unlock()
	if s := find(t, e, "s"); s.Reason != "" {
		t.Fatalf("6m under a doubled 8m N must stay down, got %s", s.Reason)
	}
	e.mu.Lock()
	e.last["s"] = time.Now().Add(-10 * time.Minute)
	e.mu.Unlock()
	if s := find(t, e, "s"); s.Reason != "stall" {
		t.Fatalf("10m past a doubled 8m N must raise again, got %q", s.Reason)
	}

	// KeepWaiting with nothing raised is a quiet no-op.
	e.mu.Lock()
	delete(e.attn, "s")
	e.mu.Unlock()
	before := len(evs)
	if err := e.KeepWaiting(t.Context(), "s"); err != nil {
		t.Fatal(err)
	}
	if evs, _ = e.Journal.Replay("s"); len(evs) != before {
		t.Fatalf("a no-op KeepWaiting must not journal, have %d events, had %d", len(evs), before)
	}
}

func TestStallNAdaptsToTheBrain(t *testing.T) {
	e := stallEngine(t)

	// Cold start: shipped defaults, local models get the long leash.
	e.mu.Lock()
	if n := e.stallN("codex"); n != 4*time.Minute {
		t.Fatalf("cold codex N = 4m, got %v", n)
	}
	if n := e.stallN("qwen-local"); n != 8*time.Minute {
		t.Fatalf("unknown brains read as local models, N = 8m, got %v", n)
	}

	// The spec's worked numbers: p95 of 71s makes N 213s.
	gaps := make([]float64, 500)
	for i := range gaps {
		gaps[i] = 1.8
	}
	for i := 450; i < 500; i++ {
		gaps[i] = 71
	}
	e.gaps["codex"] = &gapStats{Gaps: gaps}
	if n := e.stallN("codex"); n != 213*time.Second {
		t.Fatalf("p95 71s means N 213s, got %v", n)
	}

	// A near-continuous brain hits the floor, a slow one the ceiling.
	fast := make([]float64, 100)
	for i := range fast {
		fast[i] = 9
	}
	e.gaps["claude"] = &gapStats{Gaps: fast}
	if n := e.stallN("claude"); n != stallFloor {
		t.Fatalf("27s computed clamps to the 90s floor, got %v", n)
	}
	slow := make([]float64, 100)
	for i := range slow {
		slow[i] = 400
	}
	e.gaps["qwen-local"] = &gapStats{Gaps: slow}
	if n := e.stallN("qwen-local"); n != stallCeil {
		t.Fatalf("20m computed clamps to the 15m ceiling, got %v", n)
	}
	e.mu.Unlock()
}

func TestGapWindowPersistsAcrossBoots(t *testing.T) {
	root := t.TempDir()
	j, err := journal.NewFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	e := New(j)

	// A turn's worth of observed rhythm, saved the way pump end does.
	e.mu.Lock()
	e.last["s"] = time.Now().Add(-2 * time.Second)
	e.mu.Unlock()
	for range 60 {
		e.beat("s", "codex")
		e.mu.Lock()
		e.last["s"] = time.Now().Add(-2 * time.Second)
		e.mu.Unlock()
	}
	e.saveGaps("codex")

	j2, err := journal.NewFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	fresh := New(j2)
	fresh.mu.Lock()
	defer fresh.mu.Unlock()
	if got := len(fresh.gapsFor("codex").Gaps); got != 60 {
		t.Fatalf("a fresh boot must not start naive, loaded %d gaps", got)
	}
	// 60 gaps of ~2s: p95*3 is ~6s, clamped up to the floor.
	if n := fresh.stallN("codex"); n != stallFloor {
		t.Fatalf("loaded rhythm must drive N, got %v", n)
	}
}

func TestNiceDur(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{9 * time.Minute, "9m"},
		{9*time.Minute + 40*time.Second, "9m"},
		{75 * time.Minute, "1h15m"},
	} {
		if got := niceDur(tc.d); got != tc.want {
			t.Errorf("niceDur(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
