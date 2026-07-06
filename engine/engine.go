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
	nonce    uint64
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
	return &Engine{
		Journal:  j,
		seq:      map[waggle.SessionID]uint64{},
		state:    map[waggle.SessionID]hive.State{},
		running:  map[waggle.SessionID]*turn{},
		watchers: map[waggle.SessionID]map[chan waggle.Event]struct{}{},
		bases:    map[waggle.SessionID]*baseline{},
	}
}

// Sessions lists known sessions, newest first.
func (e *Engine) Sessions(ctx context.Context) ([]hive.SessionInfo, error) {
	metas, err := e.Journal.List()
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]hive.SessionInfo, 0, len(metas))
	for _, m := range metas {
		st, ok := e.state[m.ID]
		if !ok {
			st = hive.StateIdle
		}
		out = append(out, hive.SessionInfo{
			ID: m.ID, Title: m.Title, Dir: m.Dir, Brain: m.Brain,
			State: st, Created: m.Created, Updated: m.Updated,
			InRepo: inRepo(m.Dir),
		})
	}
	return out, nil
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
	st, ok := e.state[m.ID]
	e.mu.Unlock()
	if !ok {
		st = hive.StateIdle
	}
	return hive.SessionInfo{ID: m.ID, Title: m.Title, Dir: m.Dir, Brain: m.Brain, State: st, Created: m.Created, Updated: m.Updated, InRepo: inRepo(m.Dir)}
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
	e.state[id] = hive.StateWorking
	e.mu.Unlock()

	// The baseline must exist before the brain can touch a file: diff
	// and undo are promises made at the first turn, not at review time.
	if _, err := e.ensureBaseline(ctx, id); err != nil {
		e.setState(id, hive.StateDied)
		return err
	}

	e.append(waggle.Event{Sess: id, Bee: "human", Kind: waggle.KindMessage, At: time.Now(), Data: waggle.Enc(waggle.Message{Text: msg})})

	if m.Title == "" {
		m.Title = title(msg)
	}

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
	for ev := range stream.Events() {
		ev.Sess = id
		switch ev.Kind {
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
		case waggle.KindNeedInput:
			e.setState(id, hive.StateNeeds)
		}
		e.append(ev)
	}
	m.Updated = time.Now()
	_ = e.Journal.SaveMeta(m)
	e.mu.Lock()
	delete(e.running, id)
	e.state[id] = final
	e.mu.Unlock()
	close(t.done)
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
