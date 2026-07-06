package engine

// Restore means: this path becomes byte-identical to its state at
// baseline. Tracked bytes come from the baseline commit, untracked and
// non-git bytes from the blob store, and files the session created are
// deleted. A blanket restore never silently overwrites files the human
// changed after the agent, and never un-keeps what the human accepted;
// naming a path explicitly is the confirmation that overrides both.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

// RestoreSkip and RestoreReport are the hive types; the engine keeps the
// names so callers inside the module read naturally.
type (
	RestoreSkip   = hive.RestoreSkip
	RestoreReport = hive.RestoreReport
)

// RestoreAll puts every session-changed path back to its baseline state.
func (e *Engine) RestoreAll(ctx context.Context, id waggle.SessionID) (RestoreReport, error) {
	return e.Restore(ctx, id, nil)
}

// Restore implements hive.Service. Nil paths means the whole change set
// with the protective skips; explicit paths restore exactly those, even
// flagged or kept ones, because the caller named them on a screen.
func (e *Engine) Restore(ctx context.Context, id waggle.SessionID, paths []string) (RestoreReport, error) {
	b, err := e.ensureBaseline(ctx, id)
	if err != nil {
		return RestoreReport{}, err
	}
	var only map[string]bool
	force := false
	if len(paths) > 0 {
		only = map[string]bool{}
		for _, p := range paths {
			only[p] = true
		}
		force = true
	}
	var rep RestoreReport
	if b.meta.Root != "" {
		rep, err = e.restoreGit(ctx, b, only, force)
	} else {
		rep, err = e.restoreNonGit(b, only, force)
	}
	if err == nil && len(rep.Restored) > 0 {
		// Replay must show the undo happened; the tree alone cannot,
		// that being the whole point of a byte-identical restore.
		e.ensureSeq(id)
		e.append(waggle.Event{Sess: id, Bee: "hachi", Kind: waggle.KindFinding, At: time.Now(),
			Data: waggle.Enc(waggle.Message{Text: restoreMarker(rep)})})
	}
	// Undoing changes may clear the unreviewed flag a queued message
	// in the same folder was waiting on.
	go e.dispatchQueued()
	return rep, err
}

func restoreMarker(rep RestoreReport) string {
	files := "files"
	if len(rep.Restored) == 1 {
		files = "file"
	}
	s := fmt.Sprintf("undo: put %d %s back to their pre-session state", len(rep.Restored), files)
	if len(rep.Restored) == 1 {
		s = "undo: put " + rep.Restored[0] + " back to its pre-session state"
	}
	if n := len(rep.Skipped); n > 0 {
		s += fmt.Sprintf(", left %d alone", n)
	}
	return s
}

// wanted gates a path against an explicit restore list.
func wanted(only map[string]bool, rel string) bool {
	return only == nil || only[rel]
}

func (e *Engine) restoreGit(ctx context.Context, b *baseline, only map[string]bool, force bool) (RestoreReport, error) {
	var rep RestoreReport
	root := b.meta.Root
	oid := b.meta.BaselineOID

	changed, err := gitStatusPairs(ctx, root, oid)
	if err != nil {
		return rep, err
	}
	untrackedNow, err := gitPaths(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return rep, err
	}

	// Work out every target first so case collisions (one file on a Mac,
	// two paths in git) can be refused as a pair instead of restored in
	// an order that corrupts one of them.
	type action struct {
		path string
		run  func() error
	}
	var acts []action
	handled := map[string]bool{}

	for _, ch := range changed {
		st, rel := ch[0], ch[1]
		handled[rel] = true
		if !wanted(only, rel) {
			continue
		}
		if en := b.byPath[rel]; !force && b.outsideEdited(en) {
			rep.Skipped = append(rep.Skipped, RestoreSkip{Path: rel, Reason: "you changed it yourself"})
			continue
		}
		switch st {
		case "M", "D", "T":
			acts = append(acts, action{rel, func() error {
				if err := e.unstageIfStaged(ctx, b, rel); err != nil {
					return err
				}
				return restoreTracked(ctx, root, oid, rel)
			}})
		case "A":
			// Tracked now, absent from the baseline tree: the session
			// git-added it. An unborn repo's own staged files live in
			// the manifest and only need their bytes back.
			en := b.byPath[rel]
			switch {
			case en != nil && b.meta.UnbornAtBaseline && en.Class == "untracked":
				acts = append(acts, action{rel, func() error { return b.restoreBlobEntry(en) }})
			case en != nil && en.Class == "untracked":
				acts = append(acts, action{rel, func() error {
					if _, err := gitOut(ctx, root, "rm", "--cached", "-q", "--ignore-unmatch", "--", rel); err != nil {
						return err
					}
					b.clearStaged(rel)
					return b.restoreBlobEntry(en)
				}})
			default:
				acts = append(acts, action{rel, func() error {
					if _, err := gitOut(ctx, root, "rm", "--cached", "-q", "--ignore-unmatch", "--", rel); err != nil {
						return err
					}
					b.clearStaged(rel)
					return removeIgnoreMissing(filepath.Join(root, filepath.FromSlash(rel)))
				}})
			}
		}
	}

	// Untracked at baseline: restore what drifted from the manifest.
	for _, en := range b.entries {
		if en.Class != "untracked" || handled[en.Path] || !wanted(only, en.Path) {
			continue
		}
		drifted, err := b.entryDrifted(en)
		if err != nil || !drifted {
			continue
		}
		handled[en.Path] = true
		if !force && b.outsideEdited(en) {
			rep.Skipped = append(rep.Skipped, RestoreSkip{Path: en.Path, Reason: "you changed it yourself"})
			continue
		}
		if !en.Snapshotted {
			rep.Skipped = append(rep.Skipped, RestoreSkip{Path: en.Path, Reason: "too big to have saved a copy"})
			continue
		}
		en := en
		acts = append(acts, action{en.Path, func() error { return b.restoreBlobEntry(en) }})
	}

	// Untracked now but not at baseline: the session created it.
	for _, rel := range untrackedNow {
		if handled[rel] || !wanted(only, rel) {
			continue
		}
		if en, ok := b.byPath[rel]; ok && en.Class == "untracked" {
			continue // unchanged baseline scratch file, not ours to touch
		}
		if en := b.byPath[rel]; !force && b.outsideEdited(en) {
			rep.Skipped = append(rep.Skipped, RestoreSkip{Path: rel, Reason: "you changed it yourself"})
			continue
		}
		rel := rel
		acts = append(acts, action{rel, func() error {
			b.clearStaged(rel)
			return removeIgnoreMissing(filepath.Join(root, filepath.FromSlash(rel)))
		}})
	}

	rep.Skipped = append(rep.Skipped, caseCollisions(acts, func(a action) string { return a.path })...)
	collided := map[string]bool{}
	for _, s := range rep.Skipped {
		collided[s.Path] = true
	}
	for _, a := range acts {
		if collided[a.path] {
			continue
		}
		if err := a.run(); err != nil {
			return rep, fmt.Errorf("engine: restoring %s: %w", a.path, err)
		}
		rep.Restored = append(rep.Restored, a.path)
	}
	_ = b.saveManifest()
	return rep, nil
}

// unstageIfStaged puts the index entry for a path hachi staged back to
// its baseline state, so the index never holds content the tree no
// longer has. Paths the user staged themselves are never touched.
func (e *Engine) unstageIfStaged(ctx context.Context, b *baseline, rel string) error {
	en := b.byPath[rel]
	if en == nil || !en.Staged {
		return nil
	}
	// The stash commit's second parent is the index as it stood at
	// baseline; a clean baseline's index matched HEAD.
	src := b.meta.BaselineOID
	if !b.meta.CleanAtBaseline && !b.meta.UnbornAtBaseline {
		src += "^2"
	}
	if _, err := gitOut(ctx, b.meta.Root, "restore", "--staged", "--source="+src, "--", rel); err != nil {
		// The path did not exist in the baseline index at all.
		if _, err := gitOut(ctx, b.meta.Root, "rm", "--cached", "-q", "--ignore-unmatch", "--", rel); err != nil {
			return err
		}
	}
	en.Staged = false
	return nil
}

func (b *baseline) clearStaged(rel string) {
	if en := b.byPath[rel]; en != nil {
		en.Staged = false
	}
}

// entryDrifted reports whether an untracked-at-baseline file no longer
// matches its manifest record.
func (b *baseline) entryDrifted(en *ManifestEntry) (bool, error) {
	abs := filepath.Join(b.root(), filepath.FromSlash(en.Path))
	st, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil // gone now
		}
		return false, err
	}
	if en.LinkTarget != "" {
		target, err := os.Readlink(abs)
		return err != nil || target != en.LinkTarget, nil
	}
	if !st.Mode().IsRegular() || st.Mode() != en.Mode {
		return true, nil
	}
	sum, err := hashFile(abs)
	if err != nil {
		return false, err
	}
	return sum != en.SHA256, nil
}

// restoreBlobEntry writes a manifest entry's saved bytes back, verified
// by re-hash, so "restored" is a checked claim.
func (b *baseline) restoreBlobEntry(en *ManifestEntry) error {
	abs := filepath.Join(b.root(), filepath.FromSlash(en.Path))
	if en.LinkTarget != "" {
		if err := removeIgnoreMissing(abs); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		return os.Symlink(en.LinkTarget, abs)
	}
	data, err := os.ReadFile(filepath.Join(b.dir, filepath.FromSlash(en.Blob)))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	// Setuid and setgid are recorded but deliberately not restored.
	mode := en.Mode.Perm()
	if err := writeFileAtomic(abs, data, mode); err != nil {
		return err
	}
	sum, err := hashFile(abs)
	if err != nil {
		return err
	}
	if sum != en.SHA256 {
		return fmt.Errorf("restored %s does not hash to its manifest record", en.Path)
	}
	if !en.MTime.IsZero() {
		_ = os.Chtimes(abs, en.MTime, en.MTime)
	}
	return nil
}

// restoreTracked puts one tracked path back to the baseline commit's
// bytes and mode. Symlinks are recreated from the blob's target, never
// followed.
func restoreTracked(ctx context.Context, root, oid, rel string) error {
	tree, err := gitOut(ctx, root, "ls-tree", oid, "--", rel)
	if err != nil {
		return err
	}
	if tree == "" {
		return fmt.Errorf("%s not in baseline tree %s", rel, oid)
	}
	fields := strings.Fields(strings.SplitN(tree, "\t", 2)[0])
	if len(fields) < 3 {
		return fmt.Errorf("unexpected ls-tree output %q", tree)
	}
	mode := fields[0]
	data, err := gitBlob(ctx, root, oid, rel)
	if err != nil {
		return err
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	if mode == "120000" {
		if err := removeIgnoreMissing(abs); err != nil {
			return err
		}
		return os.Symlink(string(data), abs)
	}
	perm := os.FileMode(0o644)
	if mode == "100755" {
		perm = 0o755
	}
	// A same-named file may sit there with the wrong mode or as a link.
	if st, err := os.Lstat(abs); err == nil && !st.Mode().IsRegular() {
		if err := removeIgnoreMissing(abs); err != nil {
			return err
		}
	}
	if err := writeFileAtomic(abs, data, perm); err != nil {
		return err
	}
	return os.Chmod(abs, perm)
}

func (e *Engine) restoreNonGit(b *baseline, only map[string]bool, force bool) (RestoreReport, error) {
	var rep RestoreReport
	var failed []*ManifestEntry

	restoreOriginal := func(en *ManifestEntry) error { return b.restoreBlobEntry(en) }

	// Originals first, created deletions second, empty created dirs
	// last. One retry pass covers the file-replaced-by-directory case,
	// where a created dir blocks an original until deletions run.
	for _, en := range b.entries {
		if en.Class != "original" || !wanted(only, en.Path) {
			continue
		}
		if !force {
			if b.outsideEdited(en) {
				rep.Skipped = append(rep.Skipped, RestoreSkip{Path: en.Path, Reason: "you changed it yourself"})
				continue
			}
			if en.Staged {
				rep.Skipped = append(rep.Skipped, RestoreSkip{Path: en.Path, Reason: "you kept it"})
				continue
			}
		}
		if !en.Snapshotted {
			rep.Skipped = append(rep.Skipped, RestoreSkip{Path: en.Path, Reason: "too big to have saved a copy"})
			continue
		}
		en.Staged = false
		if err := restoreOriginal(en); err != nil {
			failed = append(failed, en)
			continue
		}
		rep.Restored = append(rep.Restored, en.Path)
	}
	var createdDirs []string
	for _, en := range b.entries {
		if en.Class != "created" || !wanted(only, en.Path) {
			continue
		}
		if !force {
			if b.outsideEdited(en) {
				rep.Skipped = append(rep.Skipped, RestoreSkip{Path: en.Path, Reason: "you changed it yourself"})
				continue
			}
			if en.Staged {
				rep.Skipped = append(rep.Skipped, RestoreSkip{Path: en.Path, Reason: "you kept it"})
				continue
			}
		}
		en.Staged = false
		abs := filepath.Join(b.root(), filepath.FromSlash(en.Path))
		if err := removeIgnoreMissing(abs); err != nil {
			return rep, fmt.Errorf("engine: removing %s: %w", en.Path, err)
		}
		rep.Restored = append(rep.Restored, en.Path)
		createdDirs = append(createdDirs, filepath.Dir(abs))
	}
	for _, en := range failed {
		if err := restoreOriginal(en); err != nil {
			return rep, fmt.Errorf("engine: restoring %s: %w", en.Path, err)
		}
		rep.Restored = append(rep.Restored, en.Path)
	}
	for _, dir := range createdDirs {
		pruneEmptyDirs(dir, b.root())
	}
	_ = b.saveManifest()
	return rep, nil
}

// pruneEmptyDirs removes now-empty directories from dir up toward root,
// stopping at the first non-empty one.
func pruneEmptyDirs(dir, root string) {
	for {
		if dir == root || !strings.HasPrefix(dir, root+string(filepath.Separator)) {
			return
		}
		ents, err := os.ReadDir(dir)
		if err != nil || len(ents) > 0 {
			return
		}
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// caseCollisions returns skips for path groups that collide on a
// case-insensitive filesystem, where restoring both in either order
// corrupts one of them. Only darwin defaults to such a filesystem.
func caseCollisions[T any](items []T, path func(T) string) []RestoreSkip {
	if runtime.GOOS != "darwin" {
		return nil
	}
	lower := map[string][]string{}
	for _, it := range items {
		p := path(it)
		lower[strings.ToLower(p)] = append(lower[strings.ToLower(p)], p)
	}
	var skips []RestoreSkip
	for _, group := range lower {
		if len(group) < 2 {
			continue
		}
		for _, p := range group {
			skips = append(skips, RestoreSkip{Path: p, Reason: "these names are the same file on this Mac"})
		}
	}
	return skips
}

func removeIgnoreMissing(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// gitStatusPairs parses `git diff --no-renames --name-status -z` into
// (status, path) pairs.
func gitStatusPairs(ctx context.Context, root, oid string) ([][2]string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--no-renames", "--name-status", "-z", oid)
	cmd.Dir = root
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git diff --name-status: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	fields := strings.Split(out.String(), "\x00")
	var pairs [][2]string
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "" {
			break
		}
		pairs = append(pairs, [2]string{fields[i], fields[i+1]})
	}
	return pairs, nil
}

// gitBlob returns the baseline commit's bytes for one path, untrimmed.
func gitBlob(ctx context.Context, root, oid, rel string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", oid+":"+rel)
	cmd.Dir = root
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git cat-file %s:%s: %w: %s", oid, rel, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}
