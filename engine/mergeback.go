package engine

// Bringing a worktree session's branch home. Merge-back runs in the
// user's own checkout, never in the worktree: fast-forward when the
// checkout has not moved, a real merge commit when it has. Three rules
// keep it safe. It refuses while the checkout has uncommitted changes
// in files the branch touches, naming them, because stashing the user's
// own work is not hachi's call. A conflict is aborted on the spot and
// the branch survives untouched. And cleanup only follows a clean
// merge: worktree removed, branch deleted, session back in place.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

// MergeBack implements hive.Service.
func (e *Engine) MergeBack(ctx context.Context, id waggle.SessionID) (hive.MergeReport, error) {
	m, err := e.Journal.LoadMeta(id)
	if err != nil {
		return hive.MergeReport{}, fmt.Errorf("engine: unknown session %s: %w", id, err)
	}
	if m.WorktreeBranch == "" {
		return hive.MergeReport{}, fmt.Errorf("engine: session %s works in place; there is nothing to merge back", id)
	}
	e.mu.Lock()
	_, busy := e.running[id]
	e.mu.Unlock()
	if busy {
		return hive.MergeReport{}, fmt.Errorf("engine: a turn is still running; let it finish or stop it first")
	}
	rep := hive.MergeReport{Branch: m.WorktreeBranch}

	// The user's checkout is the linked worktree's common ground: the
	// shared .git directory's parent.
	common, err := gitOut(ctx, m.WorktreePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return rep, fmt.Errorf("engine: finding the main checkout: %w", err)
	}
	root := filepath.Dir(common)

	ahead, err := gitOut(ctx, root, "rev-list", "--count", "HEAD.."+m.WorktreeBranch)
	if err != nil {
		return rep, fmt.Errorf("engine: comparing %s with the checkout: %w", m.WorktreeBranch, err)
	}
	if ahead == "0" {
		rep.Detail = fmt.Sprintf("no commits on %s yet; commit first, then merge", m.WorktreeBranch)
		return rep, nil
	}

	touched, err := gitZ(ctx, root, "diff", "--name-only", "-z", "HEAD..."+m.WorktreeBranch)
	if err != nil {
		return rep, err
	}
	dirty, err := dirtyPaths(ctx, root)
	if err != nil {
		return rep, err
	}
	for _, p := range touched {
		if dirty[p] {
			rep.Blocked = append(rep.Blocked, p)
		}
	}
	if len(rep.Blocked) > 0 {
		sort.Strings(rep.Blocked)
		rep.Detail = fmt.Sprintf("your checkout has uncommitted changes in %s; commit or stash them first", joinAnd(rep.Blocked))
		return rep, nil
	}

	if _, err := gitOut(ctx, root, "merge", "--no-edit", m.WorktreeBranch); err != nil {
		// Abort unwinds a half-applied merge; when the merge never
		// started (an overwrite refusal) there is nothing to abort and
		// the error is expected noise.
		_, _ = gitOut(ctx, root, "merge", "--abort")
		rep.Conflict = true
		rep.Detail = fmt.Sprintf("the merge conflicts with your checkout and was aborted; by hand it is git merge %s, and git merge --abort backs out", m.WorktreeBranch)
		return rep, nil
	}
	rep.Merged = true
	cur, _ := gitOut(ctx, root, "rev-parse", "--abbrev-ref", "HEAD")
	if cur == "" {
		cur = "your checkout"
	}
	rep.Detail = fmt.Sprintf("merged %s into %s", m.WorktreeBranch, cur)

	// Cleanup follows the merge, and only when git agrees the worktree
	// is disposable: remove refuses a dirty tree, and leftovers the user
	// never accepted are not hachi's to force away.
	if _, err := gitOut(ctx, root, "worktree", "remove", m.WorktreePath); err != nil {
		rep.Detail += "; the private copy still has unsaved changes, so it stays"
	} else {
		_, _ = gitOut(ctx, root, "branch", "-d", m.WorktreeBranch)
		dir := root
		if rel, err := filepath.Rel(resolved(m.WorktreePath), resolved(m.Dir)); err == nil && rel != "." && !escapesRoot(rel) {
			dir = filepath.Join(root, rel)
		}
		m.Dir, m.WorktreePath, m.WorktreeBranch = dir, "", ""
		m.Updated = time.Now()
		if err := e.Journal.SaveMeta(m); err != nil {
			return rep, err
		}
		// The baseline anchored in the removed worktree. Everything it
		// protected is committed and merged, so the next turn re-anchors
		// in the checkout with a fresh one.
		e.mu.Lock()
		delete(e.bases, id)
		e.mu.Unlock()
		_ = os.RemoveAll(filepath.Join(e.Journal.SessionDir(id), "baseline"))
		rep.Cleaned = true
	}

	e.ensureSeq(id)
	e.append(waggle.Event{Sess: id, Bee: "hachi", Kind: waggle.KindFinding, At: time.Now(),
		Data: waggle.Enc(waggle.Message{Text: rep.Detail})})
	return rep, nil
}

// gitZ runs a git command whose output is NUL-separated paths.
func gitZ(ctx context.Context, dir string, args ...string) ([]string, error) {
	out, err := gitOut(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for p := range strings.SplitSeq(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// dirtyPaths reads every path the checkout has local changes in, staged,
// unstaged, or untracked alike: all of them are user state a merge must
// not run over.
func dirtyPaths(ctx context.Context, root string) (map[string]bool, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain=v1", "-z", "--no-renames")
	cmd.Dir = root
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	// Raw stdout: a record whose first column is blank starts with a
	// space, and trimming would shift the path read below.
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git status: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	dirty := map[string]bool{}
	for entry := range strings.SplitSeq(out.String(), "\x00") {
		if len(entry) >= 4 {
			dirty[entry[3:]] = true
		}
	}
	return dirty, nil
}
