package engine

// Restore means: this path becomes byte-identical to its state at
// baseline. Tracked bytes come from the baseline commit, untracked and
// non-git bytes from the blob store, and files the session created are
// deleted. Files the human changed after the agent are never silently
// overwritten; blanket restore skips them and says so.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/tamnd/hachi/waggle"
)

// RestoreSkip is one path a restore left alone, with the reason in plain
// words, ready for the completion line.
type RestoreSkip struct {
	Path   string
	Reason string
}

// RestoreReport says what a restore actually did.
type RestoreReport struct {
	Restored []string
	Skipped  []RestoreSkip
}

// RestoreAll puts every session-changed path back to its baseline state.
// Paths flagged as changed outside the session are skipped, not asked
// about; single-file prompts are the review screen's job.
func (e *Engine) RestoreAll(ctx context.Context, id waggle.SessionID) (RestoreReport, error) {
	b, err := e.ensureBaseline(ctx, id)
	if err != nil {
		return RestoreReport{}, err
	}
	if b.meta.Root != "" {
		return e.restoreGit(ctx, b)
	}
	return e.restoreNonGit(b)
}

func (e *Engine) restoreGit(ctx context.Context, b *baseline) (RestoreReport, error) {
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
	nowSet := map[string]bool{}
	for _, p := range untrackedNow {
		nowSet[p] = true
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
		if en := b.byPath[rel]; b.outsideEdited(en) {
			rep.Skipped = append(rep.Skipped, RestoreSkip{Path: rel, Reason: "you changed it yourself"})
			continue
		}
		switch st {
		case "M", "D", "T":
			acts = append(acts, action{rel, func() error { return restoreTracked(ctx, root, oid, rel) }})
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
					return b.restoreBlobEntry(en)
				}})
			default:
				acts = append(acts, action{rel, func() error {
					if _, err := gitOut(ctx, root, "rm", "--cached", "-q", "--ignore-unmatch", "--", rel); err != nil {
						return err
					}
					return removeIgnoreMissing(filepath.Join(root, filepath.FromSlash(rel)))
				}})
			}
		}
	}

	// Untracked at baseline: restore what drifted from the manifest.
	for _, en := range b.entries {
		if en.Class != "untracked" || handled[en.Path] {
			continue
		}
		drifted, err := b.entryDrifted(en)
		if err != nil || !drifted {
			continue
		}
		handled[en.Path] = true
		if b.outsideEdited(en) {
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
		if handled[rel] {
			continue
		}
		if en, ok := b.byPath[rel]; ok && en.Class == "untracked" {
			continue // unchanged baseline scratch file, not ours to touch
		}
		if en := b.byPath[rel]; b.outsideEdited(en) {
			rep.Skipped = append(rep.Skipped, RestoreSkip{Path: rel, Reason: "you changed it yourself"})
			continue
		}
		rel := rel
		acts = append(acts, action{rel, func() error {
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
	return rep, nil
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

func (e *Engine) restoreNonGit(b *baseline) (RestoreReport, error) {
	var rep RestoreReport
	var failed []*ManifestEntry

	restoreOriginal := func(en *ManifestEntry) error { return b.restoreBlobEntry(en) }

	// Originals first, created deletions second, empty created dirs
	// last. One retry pass covers the file-replaced-by-directory case,
	// where a created dir blocks an original until deletions run.
	for _, en := range b.entries {
		if en.Class != "original" {
			continue
		}
		if b.outsideEdited(en) {
			rep.Skipped = append(rep.Skipped, RestoreSkip{Path: en.Path, Reason: "you changed it yourself"})
			continue
		}
		if !en.Snapshotted {
			rep.Skipped = append(rep.Skipped, RestoreSkip{Path: en.Path, Reason: "too big to have saved a copy"})
			continue
		}
		if err := restoreOriginal(en); err != nil {
			failed = append(failed, en)
			continue
		}
		rep.Restored = append(rep.Restored, en.Path)
	}
	var createdDirs []string
	for _, en := range b.entries {
		if en.Class != "created" {
			continue
		}
		if b.outsideEdited(en) {
			rep.Skipped = append(rep.Skipped, RestoreSkip{Path: en.Path, Reason: "you changed it yourself"})
			continue
		}
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
