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
	InRepo  bool // Dir sits inside a git repository; outside one, review defaults to the sentence view
}

// FileDiff is one file's baseline-to-now change. The engine computes it;
// clients only color it. Patch holds unified hunks with no file headers,
// since the client draws its own per-file section header.
type FileDiff struct {
	Path    string // relative to the session's root, slash-separated
	Status  string // M modified, A added, D deleted
	Outside bool   // the human changed it after the agent's last touch
	Staged  bool   // accepted through hachi: git-staged, or kept in non-git mode
	Binary  bool
	NoUndo  bool   // no saved copy of the old bytes; Restore cannot cover this file
	Patch   string // unified hunks; empty when Binary or when Note explains why
	Note    string // plain sentence shown instead of a patch
}

// RestoreSkip is one path a restore left alone, reason in plain words.
type RestoreSkip struct {
	Path   string
	Reason string
}

// RestoreReport says what a restore actually did.
type RestoreReport struct {
	Restored []string
	Skipped  []RestoreSkip
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
	// Stage accepts changes: git add in a repo, a kept mark outside one.
	// Nil paths means the whole change set except files flagged as
	// changed outside the session; those need an explicit path. Returns
	// what was actually staged.
	Stage(ctx context.Context, id waggle.SessionID, paths []string) ([]string, error)
	// CommitDraft fills a commit message template from the session's own
	// events. Deterministic, never a model call, and it exists to be
	// edited before Commit.
	CommitDraft(ctx context.Context, id waggle.SessionID) (string, error)
	// Commit runs git commit with the message exactly as the human left
	// it, scoped to the paths staged through Stage; anything the user
	// staged themselves stays staged and uncommitted. The returned
	// string is git's own output, hooks included.
	Commit(ctx context.Context, id waggle.SessionID, message string) (string, error)
	// Restore puts paths back to their baseline bytes. Nil paths means
	// everything, skipping files changed outside the session; an
	// explicit path restores even a flagged file, because naming it is
	// the confirmation.
	Restore(ctx context.Context, id waggle.SessionID, paths []string) (RestoreReport, error)
}
