// Package engine runs sessions: it owns the turn loop, assigns event
// sequence numbers, persists everything through the journal, and fans
// events out to watchers. It implements hive.Service and is the only
// package that touches adapters.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tamnd/hachi/adapter"
	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/journal"
	"github.com/tamnd/hachi/waggle"
)

// Engine implements hive.Service over a journal and the adapter registry.
type Engine struct {
	Journal *journal.Files

	mu       sync.Mutex
	seq      map[waggle.SessionID]uint64
	state    map[waggle.SessionID]hive.State
	running  map[waggle.SessionID]*turn
	watchers map[waggle.SessionID]map[chan waggle.Event]struct{}
	bases    map[waggle.SessionID]*baseline
	queue    map[waggle.SessionID]string // parked messages waiting for their folder
	dirty    map[waggle.SessionID]bool   // unaccepted changes on disk; what DiffReady reports
	attn     map[waggle.SessionID]*attention
	last     map[waggle.SessionID]time.Time // last stream event; what the stall clock measures from
	boost    map[waggle.SessionID]uint      // per-turn keep-waiting presses; each one doubles N
	gaps     map[string]*gapStats           // per-brain rhythm windows, loaded lazily
	nonce    uint64

	// wtMu serializes worktree creation so racing sends cannot pick the
	// same slug or branch.
	wtMu sync.Mutex
}

// turn is one in-flight run: its stream plus a channel that closes when
// the pump has fully drained it.
type turn struct {
	stream adapter.Stream
	done   chan struct{}
}

var _ hive.Service = (*Engine)(nil)

// New builds an Engine on a journal.
func New(j *journal.Files) *Engine {
	// The stats dir exists from the start so saveGaps never creates
	// directories late in a turn; a write after the root is gone (tests
	// tearing down) fails quietly instead of resurrecting the tree.
	_ = os.MkdirAll(filepath.Join(j.Root, "stats"), 0o755)
	return &Engine{
		Journal:  j,
		seq:      map[waggle.SessionID]uint64{},
		state:    map[waggle.SessionID]hive.State{},
		running:  map[waggle.SessionID]*turn{},
		watchers: map[waggle.SessionID]map[chan waggle.Event]struct{}{},
		bases:    map[waggle.SessionID]*baseline{},
		queue:    map[waggle.SessionID]string{},
		dirty:    map[waggle.SessionID]bool{},
		attn:     map[waggle.SessionID]*attention{},
		last:     map[waggle.SessionID]time.Time{},
		boost:    map[waggle.SessionID]uint{},
		gaps:     map[string]*gapStats{},
	}
}

// Sessions lists known sessions, newest first.
func (e *Engine) Sessions(ctx context.Context) ([]hive.SessionInfo, error) {
	metas, err := e.Journal.List()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]hive.SessionInfo, 0, len(metas))
	for _, m := range metas {
		e.checkStall(m.ID, m.Brain, now)
		st, ok := e.state[m.ID]
		if !ok {
			st = hive.StateIdle
		}
		info := hive.SessionInfo{
			ID: m.ID, Title: m.Title, Dir: m.Dir, Brain: m.Brain,
			State: st, Created: m.Created, Updated: m.Updated,
			InRepo: inRepo(m.Dir), Branch: m.WorktreeBranch,
			DiffReady: e.dirty[m.ID],
		}
		fillAttention(&info, e.attn[m.ID])
		out = append(out, info)
	}
	return out, nil
}

// fillAttention folds a raised reason into the info: the reason rides
// along, and a raised session reads as needing the human. Died keeps its
// own state, since a death is more specific than needs.
func fillAttention(info *hive.SessionInfo, a *attention) {
	if a == nil {
		return
	}
	info.Reason, info.Detail, info.Raised = a.reason, a.detail, a.raised
	if info.State != hive.StateDied {
		info.State = hive.StateNeeds
	}
}

// Open returns an existing session or creates a new one.
func (e *Engine) Open(ctx context.Context, id waggle.SessionID, dir, brain string) (hive.SessionInfo, error) {
	if id != "" {
		m, err := e.Journal.LoadMeta(id)
		if err != nil {
			return hive.SessionInfo{}, fmt.Errorf("engine: unknown session %s: %w", id, err)
		}
		return e.info(m), nil
	}
	e.mu.Lock()
	e.nonce++
	id = waggle.SessionID(fmt.Sprintf("%s-%04d", time.Now().Format("20060102-150405"), e.nonce))
	e.mu.Unlock()
	m := journal.Meta{ID: id, Dir: dir, Brain: brain, Created: time.Now(), Updated: time.Now()}
	if err := e.Journal.SaveMeta(m); err != nil {
		return hive.SessionInfo{}, err
	}
	return e.info(m), nil
}

func (e *Engine) info(m journal.Meta) hive.SessionInfo {
	e.mu.Lock()
	e.checkStall(m.ID, m.Brain, time.Now())
	st, ok := e.state[m.ID]
	dirty := e.dirty[m.ID]
	// Copied, not shared: a raised stall's detail keeps growing under
	// the lock, and this reader has already let go of it.
	var a *attention
	if live := e.attn[m.ID]; live != nil {
		cp := *live
		a = &cp
	}
	e.mu.Unlock()
	if !ok {
		st = hive.StateIdle
	}
	info := hive.SessionInfo{ID: m.ID, Title: m.Title, Dir: m.Dir, Brain: m.Brain, State: st, Created: m.Created, Updated: m.Updated, InRepo: inRepo(m.Dir), Branch: m.WorktreeBranch, DiffReady: dirty}
	fillAttention(&info, a)
	return info
}

// inRepo walks up from dir looking for a .git entry, directory or file,
// so worktrees count too. A filesystem walk instead of git itself: this
// only picks a default rendering, and the baseline still asks git for
// the authoritative root when it matters.
func inRepo(dir string) bool {
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

// Send starts a turn with the human's message. One turn per session at a
// time; steering is Stop then Send, which the client hides behind one key.
func (e *Engine) Send(ctx context.Context, id waggle.SessionID, msg string) error {
	m, err := e.Journal.LoadMeta(id)
	if err != nil {
		return fmt.Errorf("engine: unknown session %s: %w", id, err)
	}
	e.ensureSeq(id)
	e.mu.Lock()
	if _, busy := e.running[id]; busy {
		e.mu.Unlock()
		return fmt.Errorf("engine: session %s already has a turn running", id)
	}
	e.mu.Unlock()

	if m.Title == "" {
		// Saved now, not at turn end: session lists and collision
		// notices need the title while the first turn is still running.
		m.Title = title(msg)
		_ = e.Journal.SaveMeta(m)
	}

	root, _ := gitOut(ctx, m.Dir, "rev-parse", "--show-toplevel")
	if root == "" {
		// No repo means no worktree to absorb a collision; a second
		// writer is refused so the client can offer the wait. Check and
		// state flip under one lock, or two racing sends both pass.
		e.wtMu.Lock()
		if om, busy := e.folderCollision(ctx, id, m.Dir); busy {
			e.wtMu.Unlock()
			return &hive.FolderBusyError{With: om.Title}
		}
		e.setState(id, hive.StateWorking)
		e.wtMu.Unlock()
	} else {
		e.setState(id, hive.StateWorking)
		// A second writer in one repo gets a private worktree before its
		// first turn, so two sessions never interleave edits in one tree.
		// One quiet line in the transcript is all the user sees of it.
		if with, upgraded := e.maybeUpgrade(ctx, &m, root); upgraded {
			text := "working in a private copy so it can't collide with another session here"
			if with != "" {
				text = fmt.Sprintf("working in a private copy so it can't collide with %q", with)
			}
			e.append(waggle.Event{Sess: id, Bee: "hachi", Kind: waggle.KindFinding, At: time.Now(),
				Data: waggle.Enc(waggle.Message{Text: text})})
		}
	}

	// The baseline must exist before the brain can touch a file: diff
	// and undo are promises made at the first turn, not at review time.
	if _, err := e.ensureBaseline(ctx, id); err != nil {
		e.setState(id, hive.StateDied)
		return err
	}

	e.append(waggle.Event{Sess: id, Bee: "human", Kind: waggle.KindMessage, At: time.Now(), Data: waggle.Enc(waggle.Message{Text: msg})})

	drv, err := adapter.Open(m.Brain)
	if err != nil {
		e.setState(id, hive.StateDied)
		return err
	}
	// Turns outlive the caller's ctx: the TUI's Send returns immediately
	// and the turn runs until done or stopped.
	stream, err := drv.Run(context.Background(), adapter.Session{ID: id, Dir: m.Dir, Resume: m.Resume}, msg)
	if err != nil {
		e.setState(id, hive.StateDied)
		return err
	}
	t := &turn{stream: stream, done: make(chan struct{})}
	e.mu.Lock()
	e.running[id] = t
	e.mu.Unlock()

	go e.pump(id, m, t)
	return nil
}

// pump drains one turn's stream into the journal and watchers.
func (e *Engine) pump(id waggle.SessionID, m journal.Meta, t *turn) {
	stream := t.stream
	final := hive.StateIdle
	// The turn's evidence for attention: a result event means a clean
	// finish (an interrupted stream never sends one), the brain's last
	// message may be a question, and a death carries its own detail.
	var sawResult bool
	var lastSay, diedDetail string
	for ev := range stream.Events() {
		ev.Sess = id
		if e.beat(id, m.Brain) {
			// The run spoke up after a stall raise and before anyone
			// acted: silent self-heal, journaled for the stats.
			e.append(waggle.Event{Sess: id, Bee: "hachi", Kind: waggle.KindMarker, At: time.Now(),
				Data: waggle.Enc(waggle.Marker{Name: "stall_selfheal"})})
		}
		switch ev.Kind {
		case waggle.KindResult:
			sawResult = true
		case waggle.KindMessage:
			var msg waggle.Message
			if err := decode(ev.Data, &msg); err == nil && ev.Bee != "human" {
				lastSay = msg.Text
			}
		case waggle.KindSpawned:
			var sp waggle.Spawned
			if err := decode(ev.Data, &sp); err == nil && sp.Resume != "" && sp.Resume != m.Resume {
				// Persist the resume handle now, not at turn end: if the
				// process dies mid-turn the next Send must still resume
				// this thread. Losing the handle would start a fresh one
				// and re-bill the whole history as uncached input.
				m.Resume = sp.Resume
				_ = e.Journal.SaveMeta(m)
			}
		case waggle.KindEdit:
			var ed waggle.Edit
			if err := decode(ev.Data, &ed); err == nil {
				// Copy-on-write and touched-file records must land
				// before the event reaches any watcher, so a client that
				// saw the edit can trust the baseline already covers it.
				for _, warn := range e.observeEdit(id, ed) {
					e.append(warn)
				}
			}
		case waggle.KindDied:
			final = hive.StateDied
			var d waggle.Died
			if err := decode(ev.Data, &d); err == nil {
				diedDetail = d.Error
			}
		case waggle.KindNeedInput:
			// A mid-turn ask from the adapter itself; codex never sends
			// one, but the state machine is ready for brains that do.
			var ni waggle.NeedInput
			if err := decode(ev.Data, &ni); err == nil {
				e.mu.Lock()
				e.attn[id] = &attention{reason: "question", detail: ni.Prompt, raised: time.Now()}
				e.mu.Unlock()
			}
			e.setState(id, hive.StateNeeds)
		}
		e.append(ev)
	}
	m.Updated = time.Now()
	_ = e.Journal.SaveMeta(m)
	e.mu.Lock()
	delete(e.boost, id) // keep-waiting doubles N for one turn only
	e.state[id] = final
	e.mu.Unlock()
	e.saveGaps(m.Brain)
	// Recompute before the done signal: Stop waits on it, and a client
	// asking for the session right after Stop must see fresh DiffReady.
	e.refreshDirty(context.Background(), id)
	if e.settle(id, final, sawResult, lastSay, diedDetail) == "question" {
		// The ask goes in the journal too, so a replay can re-raise it.
		e.append(waggle.Event{Sess: id, Bee: "hachi", Kind: waggle.KindNeedInput, At: time.Now(),
			Data: waggle.Enc(waggle.NeedInput{Prompt: question(lastSay), Origin: "engine"})})
	}
	// The turn stays registered until here so Stop doubles as a drain
	// barrier: it returns only once every write above has landed. A Send
	// arriving during this tail is refused as busy instead of racing it.
	e.mu.Lock()
	delete(e.running, id)
	e.mu.Unlock()
	close(t.done)
	// This session leaving Working may be what a queued message in the
	// same folder was waiting on.
	e.dispatchQueued()
}

// Watch replays the session and then follows it live. The returned
// channel closes when ctx ends. Replay and subscribe happen under one
// lock so no event can fall in the gap.
func (e *Engine) Watch(ctx context.Context, id waggle.SessionID) (<-chan waggle.Event, error) {
	e.mu.Lock()
	past, err := e.Journal.Replay(id)
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	sub := make(chan waggle.Event, 256)
	if e.watchers[id] == nil {
		e.watchers[id] = map[chan waggle.Event]struct{}{}
	}
	e.watchers[id][sub] = struct{}{}
	e.mu.Unlock()

	out := make(chan waggle.Event, 256)
	go func() {
		defer close(out)
		defer func() {
			e.mu.Lock()
			delete(e.watchers[id], sub)
			e.mu.Unlock()
		}()
		for _, ev := range past {
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
		for {
			select {
			case ev := <-sub:
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Stop interrupts the running turn, if any, and waits until the turn has
// fully wound down so a Send right after Stop is never rejected as busy.
func (e *Engine) Stop(ctx context.Context, id waggle.SessionID) error {
	e.mu.Lock()
	t, ok := e.running[id]
	e.mu.Unlock()
	if !ok {
		return nil
	}
	if err := t.stream.Stop(ctx); err != nil {
		return err
	}
	select {
	case <-t.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ensureSeq seeds the in-memory sequence counter from the journal the
// first time a process appends to a session. Without this a reopened
// session would restart at one, collide with the sequence numbers
// already on disk, and clients deduplicating on seq would silently drop
// every new event after replay.
func (e *Engine) ensureSeq(id waggle.SessionID) {
	e.mu.Lock()
	_, ok := e.seq[id]
	e.mu.Unlock()
	if ok {
		return
	}
	past, err := e.Journal.Replay(id)
	if err != nil {
		return
	}
	var last uint64
	for _, ev := range past {
		if ev.Seq > last {
			last = ev.Seq
		}
	}
	e.mu.Lock()
	if _, ok := e.seq[id]; !ok {
		e.seq[id] = last
	}
	e.mu.Unlock()
}

// append assigns the session sequence number, persists, and fans out.
func (e *Engine) append(ev waggle.Event) {
	e.mu.Lock()
	e.seq[ev.Sess]++
	ev.Seq = e.seq[ev.Sess]
	_ = e.Journal.Append(ev)
	for sub := range e.watchers[ev.Sess] {
		select {
		case sub <- ev:
		default: // a stalled watcher never blocks the turn
		}
	}
	e.mu.Unlock()
}

func (e *Engine) setState(id waggle.SessionID, s hive.State) {
	e.mu.Lock()
	e.state[id] = s
	if s == hive.StateWorking {
		// A turn starting is the human acting: answering the question,
		// retrying the death. Whatever was raised is resolved by it.
		delete(e.attn, id)
		// The stall clock starts with the turn, so a brain slow to say
		// its first word is already on it.
		e.last[id] = time.Now()
	}
	e.mu.Unlock()
}

// refreshDirty recomputes whether a session holds unaccepted changes.
// Called at the moments the answer can change: a turn ends, changes are
// staged, committed, or undone, or a merged worktree goes away. The
// check runs unlocked; only the flag write takes the lock.
func (e *Engine) refreshDirty(ctx context.Context, id waggle.SessionID) {
	v := e.unreviewed(ctx, id)
	e.mu.Lock()
	if v {
		e.dirty[id] = true
	} else {
		delete(e.dirty, id)
		if a := e.attn[id]; a != nil && a.reason == "diff" {
			// Nothing left to review; the raise resolved itself.
			delete(e.attn, id)
		}
	}
	e.mu.Unlock()
}

func title(msg string) string {
	t := strings.TrimSpace(strings.Split(msg, "\n")[0])
	if len(t) > 64 {
		t = t[:64]
	}
	return t
}

func decode(raw []byte, v any) error {
	if raw == nil {
		return fmt.Errorf("empty payload")
	}
	return jsonUnmarshal(raw, v)
}
