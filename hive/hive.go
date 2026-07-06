// Package hive is the service surface of hachi: the only thing a client
// (TUI today, web or automation later) is allowed to import besides
// waggle. Keeping this package small is what makes every client a thin
// client and every future transport a detail.
package hive

import (
	"context"
	"time"

	"github.com/tamnd/hachi/waggle"
)

// State is the derived state of a session, the thing board columns and
// the attention strip are computed from.
type State string

const (
	StateIdle    State = "idle"    // waiting for the human's next message
	StateWorking State = "working" // a turn is running
	StateNeeds   State = "needs"   // blocked on the human mid-run
	StateDied    State = "died"    // last turn ended abnormally
)

// SessionInfo is what lists and boards render.
type SessionInfo struct {
	ID      waggle.SessionID
	Title   string
	Dir     string
	Brain   string
	State   State
	Created time.Time
	Updated time.Time
}

// FileDiff is one file's baseline-to-now change. The engine computes it;
// clients only color it. Patch holds unified hunks with no file headers,
// since the client draws its own per-file section header.
type FileDiff struct {
	Path    string // relative to the session's root, slash-separated
	Status  string // M modified, A added, D deleted
	Outside bool   // the human changed it after the agent's last touch
	Binary  bool
	Patch   string // unified hunks; empty when Binary or when Note explains why
	Note    string // plain sentence shown instead of a patch
}

// Service is the whole API between hachi's engine and any client.
type Service interface {
	// Sessions lists known sessions, newest first.
	Sessions(ctx context.Context) ([]SessionInfo, error)
	// Open returns an existing session, or creates one in dir with the
	// given brain when id is empty.
	Open(ctx context.Context, id waggle.SessionID, dir, brain string) (SessionInfo, error)
	// Send delivers the human's message to a session, starting a turn.
	Send(ctx context.Context, id waggle.SessionID, msg string) error
	// Watch streams a session: full replay first, then live events until
	// ctx ends. The channel closes when ctx is done.
	Watch(ctx context.Context, id waggle.SessionID) (<-chan waggle.Event, error)
	// Stop interrupts the running turn, leaving the session resumable.
	Stop(ctx context.Context, id waggle.SessionID) error
	// Changes computes the session's whole change set against its
	// baseline, right now. Safe mid-run; every call recomputes, nothing
	// is cached, so the result matches the tree at the moment of asking.
	Changes(ctx context.Context, id waggle.SessionID) ([]FileDiff, error)
}
