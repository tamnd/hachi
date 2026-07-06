package engine

// Diff-so-far lives here, never in a client. One request walks the same
// ground restore does (baseline commit, blob store, manifest) and returns
// per-file unified hunks ready to color. In git mode the whole tracked
// side comes from a single git diff invocation, split per file, with a
// per-file rerun only if the split misses one.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

// Changes implements hive.Service: everything the session changed since
// its baseline, one entry per file, sorted by path.
func (e *Engine) Changes(ctx context.Context, id waggle.SessionID) ([]hive.FileDiff, error) {
	b, err := e.ensureBaseline(ctx, id)
	if err != nil {
		return nil, err
	}
	var out []hive.FileDiff
	if b.meta.Root != "" {
		out, err = gitChanges(ctx, b)
	} else {
		out, err = nonGitChanges(ctx, b)
	}
	if err != nil {
		return nil, err
	}
	// Staged is hachi's own accepted-mark, kept in the manifest; git's
	// index state is not consulted so a user's own git add stays theirs.
	for i := range out {
		if en := b.byPath[out[i].Path]; en != nil && en.Staged {
			out[i].Staged = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func gitChanges(ctx context.Context, b *baseline) ([]hive.FileDiff, error) {
	root, oid := b.meta.Root, b.meta.BaselineOID
	pairs, err := gitStatusPairs(ctx, root, oid)
	if err != nil {
		return nil, err
	}
	sections := map[string]string{}
	if len(pairs) > 0 {
		full, err := gitOut(ctx, root, "-c", "core.quotepath=false", "diff", "--no-renames", oid)
		if err != nil {
			return nil, err
		}
		sections = splitDiff(full)
	}

	var out []hive.FileDiff
	handled := map[string]bool{}
	for _, ch := range pairs {
		st, rel := ch[0], ch[1]
		handled[rel] = true
		if st == "T" {
			st = "M" // a type change reads as a modification
		}
		fd := hive.FileDiff{Path: rel, Status: st, Outside: b.outsideEdited(b.byPath[rel])}
		sec, ok := sections[rel]
		if !ok {
			// The split missed this path (odd quoting, header collision);
			// one extra git call settles it.
			sec, err = gitOut(ctx, root, "-c", "core.quotepath=false", "diff", "--no-renames", oid, "--", rel)
			if err != nil {
				return nil, err
			}
		}
		body, binary, oldMode, newMode := patchParts(sec)
		switch {
		case binary:
			fd.Binary = true
			var oldSize, newSize int64
			if s, err := gitOut(ctx, root, "cat-file", "-s", oid+":"+rel); err == nil {
				oldSize, _ = strconv.ParseInt(s, 10, 64)
			}
			if info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
				newSize = info.Size()
			}
			fd.Note = binaryNote(st, oldSize, newSize)
		default:
			fd.Patch = body
			if oldMode != "" && newMode != "" {
				fd.Note = fmt.Sprintf("mode changed (%s → %s)", oldMode, newMode)
			}
		}
		out = append(out, fd)
	}

	// Untracked at baseline: anything that drifted from its manifest
	// record changed under this session's watch.
	for _, en := range b.entries {
		if en.Class != "untracked" || handled[en.Path] {
			continue
		}
		drifted, err := b.entryDrifted(en)
		if err != nil || !drifted {
			continue
		}
		handled[en.Path] = true
		fd, err := manifestDiff(ctx, b, en)
		if err != nil {
			return nil, err
		}
		out = append(out, fd)
	}

	// Untracked now, not at baseline: the session created it.
	untrackedNow, err := gitPaths(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	for _, rel := range untrackedNow {
		if handled[rel] {
			continue
		}
		if en, ok := b.byPath[rel]; ok && en.Class == "untracked" {
			continue // unchanged baseline scratch file
		}
		fd, err := createdDiff(ctx, b, rel)
		if err != nil {
			return nil, err
		}
		out = append(out, fd)
	}
	return out, nil
}

func nonGitChanges(ctx context.Context, b *baseline) ([]hive.FileDiff, error) {
	var out []hive.FileDiff
	for _, en := range b.entries {
		switch en.Class {
		case "created":
			abs := filepath.Join(b.root(), filepath.FromSlash(en.Path))
			if _, err := os.Lstat(abs); err != nil {
				continue // created then removed again; no change left
			}
			fd, err := createdDiff(ctx, b, en.Path)
			if err != nil {
				return nil, err
			}
			out = append(out, fd)
		case "original":
			if en.SHA256 != "" || en.LinkTarget != "" {
				drifted, err := b.entryDrifted(en)
				if err != nil {
					return nil, err
				}
				if !drifted {
					continue
				}
			}
			fd, err := manifestDiff(ctx, b, en)
			if err != nil {
				return nil, err
			}
			out = append(out, fd)
		}
	}
	return out, nil
}

// manifestDiff diffs one manifest-recorded file (untracked at baseline in
// git mode, original in non-git mode) against its state on disk.
func manifestDiff(ctx context.Context, b *baseline, en *ManifestEntry) (hive.FileDiff, error) {
	fd := hive.FileDiff{Path: en.Path, Status: "M", Outside: b.outsideEdited(en)}
	abs := filepath.Join(b.root(), filepath.FromSlash(en.Path))
	st, statErr := os.Lstat(abs)
	gone := statErr != nil
	if gone {
		fd.Status = "D"
	}
	switch {
	case en.LinkTarget != "":
		if gone {
			fd.Note = fmt.Sprintf("symlink removed (pointed to %s)", en.LinkTarget)
		} else if t, err := os.Readlink(abs); err == nil {
			fd.Note = fmt.Sprintf("symlink now points to %s (was %s)", t, en.LinkTarget)
		} else {
			fd.Note = fmt.Sprintf("was a symlink to %s, now a regular file", en.LinkTarget)
		}
	case !en.Snapshotted:
		fd.NoUndo = true
		if en.SHA256 != "" {
			fd.Note = "changed, but the old version was too big to keep a copy of"
		} else {
			fd.Note = "changed before a copy of the old version could be saved"
		}
	default:
		blob := filepath.Join(b.dir, filepath.FromSlash(en.Blob))
		newPath := abs
		if gone {
			newPath = ""
		}
		body, binary, err := noIndexPatch(ctx, blob, newPath)
		if err != nil {
			return fd, err
		}
		if binary {
			fd.Binary = true
			var newSize int64
			if !gone {
				newSize = st.Size()
			}
			fd.Note = binaryNote(fd.Status, en.Size, newSize)
		} else {
			fd.Patch = body
		}
	}
	return fd, nil
}

// createdDiff renders a file the session brought into being: everything
// is an addition.
func createdDiff(ctx context.Context, b *baseline, rel string) (hive.FileDiff, error) {
	fd := hive.FileDiff{Path: rel, Status: "A", Outside: b.outsideEdited(b.byPath[rel])}
	abs := filepath.Join(b.root(), filepath.FromSlash(rel))
	st, err := os.Lstat(abs)
	if err != nil {
		return fd, err
	}
	if st.Mode()&fs.ModeSymlink != 0 {
		t, _ := os.Readlink(abs)
		fd.Note = "symlink to " + t
		return fd, nil
	}
	body, binary, err := noIndexPatch(ctx, "", abs)
	if err != nil {
		return fd, err
	}
	if binary {
		fd.Binary = true
		fd.Note = binaryNote("A", 0, st.Size())
	} else {
		fd.Patch = body
	}
	return fd, nil
}

// noIndexPatch diffs two files by literal path and returns hunks only.
// An empty path means that side does not exist. Exit code 1 is git for
// "they differ", which is the whole point of asking.
func noIndexPatch(ctx context.Context, aPath, bPath string) (string, bool, error) {
	if aPath == "" {
		aPath = os.DevNull
	}
	if bPath == "" {
		bPath = os.DevNull
	}
	cmd := exec.CommandContext(ctx, "git", "-c", "core.quotepath=false", "diff", "--no-index", "--", aPath, bPath)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		var xe *exec.ExitError
		if !errors.As(err, &xe) || xe.ExitCode() != 1 {
			return "", false, fmt.Errorf("git diff --no-index: %w: %s", err, strings.TrimSpace(errb.String()))
		}
	}
	body, binary, _, _ := patchParts(out.String())
	return body, binary, nil
}

// patchParts strips one file's diff section down to what a reader needs:
// the hunks, whether git called it binary, and any mode change.
func patchParts(section string) (body string, binary bool, oldMode, newMode string) {
	lines := strings.Split(section, "\n")
	for i, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "@@ "):
			return strings.TrimRight(strings.Join(lines[i:], "\n"), "\n"), false, oldMode, newMode
		case strings.HasPrefix(ln, "Binary files "):
			binary = true
		case strings.HasPrefix(ln, "old mode "):
			oldMode = strings.TrimPrefix(ln, "old mode ")
		case strings.HasPrefix(ln, "new mode "):
			newMode = strings.TrimPrefix(ln, "new mode ")
		}
	}
	return "", binary, oldMode, newMode
}

// splitDiff cuts one multi-file git diff into per-path sections. Paths
// come from the file headers, so hunk content that happens to start with
// header-looking text is ignored once hunks begin.
func splitDiff(full string) map[string]string {
	sections := map[string]string{}
	var cur []string
	var curPath string
	inHunks := false
	flush := func() {
		if curPath != "" {
			sections[curPath] = strings.Join(cur, "\n")
		}
	}
	for ln := range strings.SplitSeq(full, "\n") {
		if strings.HasPrefix(ln, "diff --git ") {
			flush()
			cur, curPath, inHunks = nil, "", false
		}
		cur = append(cur, ln)
		switch {
		case inHunks:
		case strings.HasPrefix(ln, "@@ "):
			inHunks = true
		case strings.HasPrefix(ln, "+++ "):
			if p := headerPath(strings.TrimPrefix(ln, "+++ ")); p != "" {
				curPath = p
			}
		case strings.HasPrefix(ln, "--- ") && curPath == "":
			curPath = headerPath(strings.TrimPrefix(ln, "--- "))
		}
	}
	flush()
	return sections
}

// headerPath turns a diff file header operand into a repo-relative path.
func headerPath(rest string) string {
	if strings.HasPrefix(rest, "\"") {
		if uq, err := strconv.Unquote(rest); err == nil {
			rest = uq
		}
	}
	rest = strings.TrimSuffix(rest, "\t")
	switch {
	case rest == "/dev/null":
		return ""
	case strings.HasPrefix(rest, "a/"), strings.HasPrefix(rest, "b/"):
		return rest[2:]
	}
	return rest
}

// binaryNote is the sentence shown for a file whose bytes cannot be read
// as a patch.
func binaryNote(status string, oldSize, newSize int64) string {
	switch status {
	case "A":
		return fmt.Sprintf("binary file added (%s)", humanSize(newSize))
	case "D":
		return fmt.Sprintf("binary file deleted (was %s)", humanSize(oldSize))
	}
	return fmt.Sprintf("binary file changed (%s → %s)", humanSize(oldSize), humanSize(newSize))
}

func humanSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	for _, unit := range []string{"KB", "MB", "GB", "TB"} {
		f /= 1024
		if f < 1024 {
			return fmt.Sprintf("%.1f %s", f, unit)
		}
	}
	return fmt.Sprintf("%.1f PB", f/1024)
}
