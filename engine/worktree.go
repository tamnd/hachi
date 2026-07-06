package engine

// The silent worktree upgrade. In-place work is the default for a single
// session, but the moment a second session would start writing in a repo
// that already has one working or holding unreviewed changes, the second
// session gets a private copy: a linked git worktree on its own
// hachi/<slug> branch, created from HEAD before its first turn runs. The
// first session is never moved, the user is never asked, and the only
// trace in the conversation is one quiet line. Worktrees live under the
// hive home, outside the repo, so nothing shows up in the user's checkout.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/journal"
	"github.com/tamnd/hachi/waggle"
)

// maybeUpgrade checks whether starting a turn for m would collide with
// another session in the same repo root and, if so, moves m into a fresh
// worktree before anything runs. It reports whether the upgrade happened
// and the colliding session's title. Only a session that has never
// captured a baseline can move: once a session has worked in place, its
// diff and undo promises are anchored there.
func (e *Engine) maybeUpgrade(ctx context.Context, m *journal.Meta) (string, bool) {
	if m.WorktreePath != "" {
		return "", false // already in a private copy
	}
	if e.hasBaseline(m.ID) {
		return "", false // worked in place before; it stays in place
	}
	root, err := gitOut(ctx, m.Dir, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return "", false // not a git repo; nothing to upgrade
	}
	other, ok := e.collision(ctx, m.ID, root)
	if !ok {
		return "", false
	}

	// One upgrade at a time keeps slug dedup honest when sends race.
	e.wtMu.Lock()
	defer e.wtMu.Unlock()

	base := filepath.Join(e.Journal.Root, "worktrees", repoSlug(root))
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", false
	}
	slug := slugify(m.Title)
	if slug == "" {
		slug = string(m.ID)
	}
	path, branch, ok := freeSlot(ctx, root, base, slug)
	if !ok {
		return "", false
	}
	if _, err := gitOut(ctx, root, "worktree", "add", path, "-b", branch); err != nil {
		// A repo where worktrees cannot be created (bare edge, exotic
		// filesystem) falls back to working in place; the baseline still
		// flags any cross-session interference as outside edits.
		return "", false
	}

	// The session keeps its spot relative to the root, so a session
	// opened in repo/subdir lands in worktree/subdir. Both sides resolve
	// symlinks first; git reports resolved roots while the session dir
	// keeps whatever spelling it started from.
	dir := path
	if rel, err := filepath.Rel(resolved(root), resolved(m.Dir)); err == nil && rel != "." && !escapesRoot(rel) {
		dir = filepath.Join(path, rel)
	}
	m.Dir = dir
	m.WorktreePath = path
	m.WorktreeBranch = branch
	if err := e.Journal.SaveMeta(*m); err != nil {
		return "", false
	}
	return other.Title, true
}

// collision finds a session that makes root unsafe to write in: one with
// a running turn, or one holding changes nobody has reviewed yet. Two
// idle sessions in one repo are fine; the upgrade only matters at the
// moment a second writer starts.
func (e *Engine) collision(ctx context.Context, self waggle.SessionID, root string) (journal.Meta, bool) {
	metas, err := e.Journal.List()
	if err != nil {
		return journal.Meta{}, false
	}
	canon := resolved(root)
	for _, om := range metas {
		if om.ID == self {
			continue
		}
		// Cheap filter first: only a session under this root can share
		// it. A worktree session's own root is the worktree, so upgraded
		// siblings never re-trigger.
		if !underDir(canon, resolved(om.Dir)) {
			continue
		}
		or, err := gitOut(ctx, om.Dir, "rev-parse", "--show-toplevel")
		if err != nil || resolved(or) != canon {
			continue // nested repo or the dir went away
		}
		e.mu.Lock()
		working := e.state[om.ID] == hive.StateWorking
		e.mu.Unlock()
		if working {
			return om, true
		}
		if e.unreviewed(ctx, om.ID) {
			return om, true
		}
	}
	return journal.Meta{}, false
}

// unreviewed reports whether a session left changes the human has not
// accepted. Only sessions that already captured a baseline are asked, so
// the check never captures one as a side effect.
func (e *Engine) unreviewed(ctx context.Context, id waggle.SessionID) bool {
	if !e.hasBaseline(id) {
		return false
	}
	diffs, err := e.Changes(ctx, id)
	if err != nil {
		return false
	}
	for _, d := range diffs {
		if !d.Staged {
			return true
		}
	}
	return false
}

// hasBaseline reports whether a session's baseline exists, in memory or
// on disk, without capturing one.
func (e *Engine) hasBaseline(id waggle.SessionID) bool {
	e.mu.Lock()
	_, ok := e.bases[id]
	e.mu.Unlock()
	if ok {
		return true
	}
	_, err := os.Stat(filepath.Join(e.Journal.SessionDir(id), "baseline", "meta.json"))
	return err == nil
}

// freeSlot picks the first worktree path and branch name not already
// taken, deduplicating with a numeric suffix: port-lexer-v2, then
// port-lexer-v2-2, and so on.
func freeSlot(ctx context.Context, root, base, slug string) (path, branch string, ok bool) {
	for n := 1; n <= 100; n++ {
		cand := slug
		if n > 1 {
			cand = fmt.Sprintf("%s-%d", slug, n)
		}
		path = filepath.Join(base, cand)
		branch = "hachi/" + cand
		if _, err := os.Lstat(path); err == nil {
			continue
		}
		if _, err := gitOut(ctx, root, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
			continue // branch exists from an earlier session
		}
		return path, branch, true
	}
	return "", "", false
}

// repoSlug names the per-repo worktree folder: the root's base name plus
// a short hash of its resolved path, so two checkouts of the same project
// never share a folder.
func repoSlug(root string) string {
	sum := sha256.Sum256([]byte(resolved(root)))
	s := slugify(filepath.Base(root))
	if s == "" {
		s = "repo"
	}
	return s + "-" + hex.EncodeToString(sum[:])[:6]
}

// slugify flattens a title into a branch-safe name: lowercase, runs of
// anything but letters and digits become single hyphens, capped short.
func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = strings.Trim(out[:40], "-")
	}
	return out
}

// underDir reports whether p sits at or below dir. Both must already be
// symlink-resolved.
func underDir(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && !escapesRoot(rel) && !filepath.IsAbs(rel)
}
