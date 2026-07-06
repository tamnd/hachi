package engine

// The non-git fallback for same-folder concurrency. Worktrees are a git
// feature; a plain folder gets serialization instead of sharing, because
// two writers with per-file baselines in one folder would each record
// the other's edits as outside edits and both undos would degrade to
// prompts. Send refuses the second writer with a FolderBusyError, the
// client asks the human, and Queue is the wait: the parked message
// starts the moment the folder has no working session and no unreviewed
// changes. The queue lives in memory only; a crash drops it, which is
// just the composer state it replaced.

import (
	"context"
	"errors"
	"fmt"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/journal"
	"github.com/tamnd/hachi/waggle"
)

// Queue implements hive.Service: park a message until the folder frees.
func (e *Engine) Queue(ctx context.Context, id waggle.SessionID, msg string) error {
	if _, err := e.Journal.LoadMeta(id); err != nil {
		return fmt.Errorf("engine: unknown session %s: %w", id, err)
	}
	e.mu.Lock()
	if _, busy := e.running[id]; busy {
		e.mu.Unlock()
		return fmt.Errorf("engine: session %s already has a turn running", id)
	}
	e.queue[id] = msg
	e.state[id] = hive.StateWaiting
	e.mu.Unlock()
	// The folder may have freed up during the ask; try right away.
	go e.dispatchQueued()
	return nil
}

// dispatchQueued offers every parked message to Send. Send itself is the
// gate: a message whose folder is still busy comes back as a
// FolderBusyError and returns to the queue, so dispatch never needs its
// own collision logic and never holds a lock across a turn start.
func (e *Engine) dispatchQueued() {
	e.mu.Lock()
	ids := make([]waggle.SessionID, 0, len(e.queue))
	for id := range e.queue {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	for _, id := range ids {
		e.mu.Lock()
		msg, ok := e.queue[id]
		delete(e.queue, id)
		e.mu.Unlock()
		if !ok {
			continue
		}
		err := e.Send(context.Background(), id, msg)
		if _, busy := errors.AsType[*hive.FolderBusyError](err); busy {
			e.mu.Lock()
			e.queue[id] = msg
			e.state[id] = hive.StateWaiting
			e.mu.Unlock()
		}
	}
}

// folderCollision finds an active session sharing a non-git folder with
// dir: one with a running turn, or one holding changes nobody reviewed.
// Folders overlap when one contains the other; per-file baselines track
// subtrees, so a session in a parent folder can reach a child's files.
func (e *Engine) folderCollision(ctx context.Context, self waggle.SessionID, dir string) (journal.Meta, bool) {
	metas, err := e.Journal.List()
	if err != nil {
		return journal.Meta{}, false
	}
	canon := resolved(dir)
	for _, om := range metas {
		if om.ID == self {
			continue
		}
		od := resolved(om.Dir)
		if !underDir(canon, od) && !underDir(od, canon) {
			continue
		}
		// A git repo nested in this folder manages its own concurrency
		// through worktrees.
		if r, err := gitOut(ctx, om.Dir, "rev-parse", "--show-toplevel"); err == nil && r != "" {
			continue
		}
		e.mu.Lock()
		working := e.state[om.ID] == hive.StateWorking
		e.mu.Unlock()
		if working || e.unreviewed(ctx, om.ID) {
			return om, true
		}
	}
	return journal.Meta{}, false
}
