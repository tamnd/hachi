package engine

// Stall detection answers one question: has this run gone quiet for
// longer than this brain normally goes quiet. N is derived from the
// brain's own observed rhythm, so a chatty brain pages fast and a slow
// local model gets its silences, with no config knob. The check runs on
// the client's poll instead of a timer: the answer is only visible when
// someone asks, so asking is the tick. The detector never kills
// anything; it raises attention and every follow-up is a human key.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

const (
	// stallFloor exists because paging faster than 90 seconds is nagging
	// no matter how chatty the brain; stallCeil because 15 silent minutes
	// is worth a look no matter how slow.
	stallFloor = 90 * time.Second
	stallCeil  = 15 * time.Minute

	gapKeep   = 500              // rolling window of observed inter-event gaps per brain
	gapEnough = 50               // fewer observations than this and the defaults hold
	gapCap    = 30 * time.Minute // longer reads as a suspend or a wall clock jump, not rhythm
)

// stallDefault is the cold-start N for a brain with no track record yet.
// Unlisted brains get the local-model default: a 30B model on one GPU
// legitimately thinks in silence longer, and the tail is fatter.
func stallDefault(brain string) time.Duration {
	switch brain {
	case "codex", "claude":
		return 4 * time.Minute
	case "opencode":
		return 5 * time.Minute
	}
	return 8 * time.Minute
}

// gapStats is one brain's rhythm: the last gapKeep inter-event gaps in
// seconds, newest last. Persisted per brain so a fresh boot does not
// start naive.
type gapStats struct {
	Gaps []float64 `json:"gaps"`
}

// beat notes one event arriving on a live stream: the stall clock
// resets, the brain's rhythm gains an observation, and a raised stall
// heals itself. It reports whether it healed one, so the pump can put
// the incident in the journal for the false-positive stats.
func (e *Engine) beat(id waggle.SessionID, brain string) bool {
	now := time.Now()
	e.mu.Lock()
	if prev := e.last[id]; !prev.IsZero() {
		if gap := now.Sub(prev); gap > 0 && gap < gapCap {
			g := e.gapsFor(brain)
			g.Gaps = append(g.Gaps, gap.Seconds())
			if len(g.Gaps) > gapKeep {
				g.Gaps = g.Gaps[len(g.Gaps)-gapKeep:]
			}
		}
	}
	e.last[id] = now
	healed := false
	if a := e.attn[id]; a != nil && a.reason == "stall" {
		delete(e.attn, id)
		healed = true
	}
	e.mu.Unlock()
	return healed
}

// checkStall raises or refreshes a stall for one session. Caller holds
// e.mu. The clock only runs while the turn is executing: a session
// blocked on the human mid-run reads needs, not working, so waits you
// cause never page you.
func (e *Engine) checkStall(id waggle.SessionID, brain string, now time.Time) {
	if e.state[id] != hive.StateWorking {
		return
	}
	if _, running := e.running[id]; !running {
		return
	}
	last := e.last[id]
	if last.IsZero() {
		return
	}
	quiet := now.Sub(last)
	if a := e.attn[id]; a != nil {
		if a.reason == "stall" {
			// The silence keeps growing while raised; say how long.
			a.detail = "quiet for " + niceDur(quiet)
		}
		return
	}
	if quiet > e.stallN(brain)<<e.boost[id] {
		e.attn[id] = &attention{reason: "stall", detail: "quiet for " + niceDur(quiet), raised: now}
	}
}

// stallN is the adaptive threshold: three times the brain's own p95
// gap, clamped. Caller holds e.mu.
func (e *Engine) stallN(brain string) time.Duration {
	g := e.gapsFor(brain)
	if len(g.Gaps) < gapEnough {
		return stallDefault(brain)
	}
	sorted := append([]float64{}, g.Gaps...)
	sort.Float64s(sorted)
	p95 := sorted[int(float64(len(sorted)-1)*0.95)]
	n := time.Duration(p95*3) * time.Second
	if n < stallFloor {
		return stallFloor
	}
	if n > stallCeil {
		return stallCeil
	}
	return n
}

// KeepWaiting is the human vouching for a quiet run: the raise clears,
// and this session's N doubles for the rest of the turn, so one
// keypress buys peace without disabling detection. The press itself is
// journaled, feeding the false-positive stats. A safe no-op when no
// stall is raised.
func (e *Engine) KeepWaiting(ctx context.Context, id waggle.SessionID) error {
	e.ensureSeq(id)
	e.mu.Lock()
	a := e.attn[id]
	if a == nil || a.reason != "stall" {
		e.mu.Unlock()
		return nil
	}
	delete(e.attn, id)
	e.boost[id]++
	e.last[id] = time.Now()
	e.mu.Unlock()
	e.append(waggle.Event{Sess: id, Bee: "hachi", Kind: waggle.KindMarker, At: time.Now(),
		Data: waggle.Enc(waggle.Marker{Name: "stall_wait"})})
	return nil
}

// gapsFor returns a brain's rhythm, loading the persisted window the
// first time the brain comes up in this process. Caller holds e.mu.
func (e *Engine) gapsFor(brain string) *gapStats {
	if g := e.gaps[brain]; g != nil {
		return g
	}
	g := &gapStats{}
	if raw, err := os.ReadFile(e.statsPath(brain)); err == nil {
		_ = json.Unmarshal(raw, g)
	}
	e.gaps[brain] = g
	return g
}

// saveGaps persists a brain's window; called at turn end, not per
// event, so the journal stays the only hot write path.
func (e *Engine) saveGaps(brain string) {
	e.mu.Lock()
	g := e.gaps[brain]
	var raw []byte
	if g != nil && len(g.Gaps) > 0 {
		raw, _ = json.Marshal(g)
	}
	e.mu.Unlock()
	if raw == nil {
		return
	}
	// The stats dir is made at New; no MkdirAll here, so this write
	// cannot resurrect a tree a test teardown is removing.
	_ = os.WriteFile(e.statsPath(brain), raw, 0o644)
}

func (e *Engine) statsPath(brain string) string {
	return filepath.Join(e.Journal.Root, "stats", brain+".json")
}

// niceDur says a duration the way a human would over a shoulder.
func niceDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}
