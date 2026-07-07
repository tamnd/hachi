package engine

// Deleting a session. The verb is honest: the journal directory goes,
// the pinned baseline ref goes, the private worktree goes, and there is
// no soft-delete state to manage. Two things survive on purpose. A
// branch whose commits exist nowhere else stays, because those commits
// are the user's work and hachi does not destroy work; the returned
// sentence names the branch. And the user's own checkout is never
// touched at all.

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/tamnd/hachi/waggle"
)

// Delete implements hive.Service.
func (e *Engine) Delete(ctx context.Context, id waggle.SessionID) (string, error) {
	m, err := e.Journal.LoadMeta(id)
	if err != nil {
		return "", fmt.Errorf("engine: unknown session %s: %w", id, err)
	}

	// The parked message goes before the stop: dispatch must not start a
	// fresh turn on a session that is on its way out.
	e.mu.Lock()
	delete(e.queue, id)
	e.mu.Unlock()

	// Stop is a drain barrier: when it returns, no goroutine of this
	// session's turn is still appending to the journal.
	if err := e.Stop(ctx, id); err != nil {
		return "", err
	}

	var kept string
	repoDir := m.Dir
	if m.WorktreePath != "" {
		// The branch and the baseline ref live in the user's checkout:
		// the shared .git directory's parent.
		if common, err := gitOut(ctx, m.WorktreePath, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
			repoDir = filepath.Dir(common)
		}
		// The worktree is hachi's private copy and the delete was
		// confirmed; --force covers uncommitted leftovers inside it.
		_, _ = gitOut(ctx, repoDir, "worktree", "remove", "--force", m.WorktreePath)
		if m.WorktreeBranch != "" {
			// -d only deletes a branch whose commits are reachable from
			// the checkout. When it refuses, the branch holds work that
			// exists nowhere else, so it stays and the sentence says so.
			if _, err := gitOut(ctx, repoDir, "branch", "-d", m.WorktreeBranch); err != nil {
				kept = fmt.Sprintf("its commits stay on %s", m.WorktreeBranch)
			}
		}
	}

	// The pinned baseline object is only reachable through this ref;
	// dropping it lets git gc reclaim the snapshot. Best effort: non-git
	// sessions have no ref and a vanished folder has nowhere to run git.
	_, _ = gitOut(ctx, repoDir, "update-ref", "-d", "refs/hachi/baseline/"+string(id))

	e.mu.Lock()
	delete(e.seq, id)
	delete(e.state, id)
	delete(e.watchers, id)
	delete(e.bases, id)
	delete(e.dirty, id)
	delete(e.attn, id)
	delete(e.last, id)
	delete(e.boost, id)
	e.mu.Unlock()

	if err := e.Journal.Delete(id); err != nil {
		return kept, err
	}
	return kept, nil
}
