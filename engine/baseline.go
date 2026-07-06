package engine

// Every session snapshots a baseline: a byte-accurate record of the
// working folder at the moment the first turn starts. The baseline is
// the review boundary (diff shows only this session's work, even in a
// dirty tree), and it is what makes undo byte-identical. In a git repo
// the tracked state rides a dangling stash commit and untracked files
// get content-addressed copies; outside git, files are copied the moment
// an edit event says the agent is about to touch them. The captured
// bytes never change after capture; only touched-file records are
// appended as the agent works.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tamnd/hachi/waggle"
)

// emptyTreeOID is git's well-known empty tree, the baseline for a repo
// with no commits yet.
const emptyTreeOID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Snapshot size caps. Files over the per-file cap are recorded but not
// copied, so undo cannot cover them and review says so. The session cap
// stops further copies without stopping the session.
const (
	maxBlobBytes    = 8 << 20
	maxSessionBytes = 512 << 20
)

// BaselineMeta is the durable record of a session's starting state. It
// is written once at capture and read-only after.
type BaselineMeta struct {
	SessionID string    `json:"session_id"`
	TakenAt   time.Time `json:"taken_at"`

	// Git mode. Empty Root means non-git mode.
	Root             string `json:"git_root,omitempty"`
	BaselineOID      string `json:"baseline_oid,omitempty"`
	HeadOID          string `json:"head_oid,omitempty"`
	CleanAtBaseline  bool   `json:"clean_at_baseline,omitempty"`
	UnbornAtBaseline bool   `json:"unborn_at_baseline,omitempty"`

	// Both modes.
	Dir string `json:"dir"`
}

// ManifestEntry is one file's record in the baseline manifest. At
// capture it describes untracked files (git mode); as the session runs,
// touched-file records are appended (non-git originals and created
// files, plus what the agent left behind per edit event).
type ManifestEntry struct {
	Path        string      `json:"path"`  // relative to the baseline root, slash-separated
	Class       string      `json:"class"` // untracked | original | created
	Size        int64       `json:"size"`
	Mode        fs.FileMode `json:"mode"`
	MTime       time.Time   `json:"mtime"`
	SHA256      string      `json:"sha256,omitempty"` // empty for symlinks
	LinkTarget  string      `json:"link_target,omitempty"`
	Snapshotted bool        `json:"snapshotted"`
	Blob        string      `json:"blob,omitempty"`

	// Filled in as the agent works: what the file looked like when the
	// last edit event for it completed. Empty SHA with a set MTime means
	// the agent deleted the file.
	LastAgentSHA   string    `json:"last_agent_sha,omitempty"`
	LastAgentMTime time.Time `json:"last_agent_mtime,omitzero"`
}

// baseline is the in-memory handle for one session's snapshot.
type baseline struct {
	meta      BaselineMeta
	dir       string // <session dir>/baseline
	entries   []*ManifestEntry
	byPath    map[string]*ManifestEntry
	blobBytes int64
}

// root is the directory manifest paths are relative to: the repo root in
// git mode, the session folder otherwise.
func (b *baseline) root() string {
	if b.meta.Root != "" {
		return b.meta.Root
	}
	return b.meta.Dir
}

// Baseline returns the session's baseline, capturing it now if this is
// the first time anything asked. The engine calls it at the start of the
// first turn; tests and future review plumbing call it directly.
func (e *Engine) Baseline(ctx context.Context, id waggle.SessionID) (BaselineMeta, error) {
	b, err := e.ensureBaseline(ctx, id)
	if err != nil {
		return BaselineMeta{}, err
	}
	return b.meta, nil
}

func (e *Engine) ensureBaseline(ctx context.Context, id waggle.SessionID) (*baseline, error) {
	e.mu.Lock()
	if b, ok := e.bases[id]; ok {
		e.mu.Unlock()
		return b, nil
	}
	e.mu.Unlock()

	m, err := e.Journal.LoadMeta(id)
	if err != nil {
		return nil, fmt.Errorf("engine: baseline for unknown session %s: %w", id, err)
	}
	dir := filepath.Join(e.Journal.SessionDir(id), "baseline")
	b, err := loadBaseline(dir)
	if err == nil {
		return e.adoptBaseline(id, b), nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	b, err = captureBaseline(ctx, id, m.Dir, dir)
	if err != nil {
		return nil, err
	}
	return e.adoptBaseline(id, b), nil
}

// adoptBaseline publishes a baseline handle, keeping the first one if
// two turns raced to capture.
func (e *Engine) adoptBaseline(id waggle.SessionID, b *baseline) *baseline {
	e.mu.Lock()
	defer e.mu.Unlock()
	if prev, ok := e.bases[id]; ok {
		return prev
	}
	e.bases[id] = b
	return b
}

// captureBaseline records the folder's state. Under 100ms on a normal
// repo, and it writes nothing to the user's tree, index, stash, or
// reflog: the stash commit dangles and the pin ref lives under
// refs/hachi/, invisible to porcelain.
func captureBaseline(ctx context.Context, id waggle.SessionID, workDir, baseDir string) (*baseline, error) {
	b := &baseline{
		meta:   BaselineMeta{SessionID: string(id), TakenAt: time.Now(), Dir: workDir},
		dir:    baseDir,
		byPath: map[string]*ManifestEntry{},
	}
	if root, err := gitOut(ctx, workDir, "rev-parse", "--show-toplevel"); err == nil && root != "" {
		if err := b.captureGit(ctx, root); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Join(baseDir, "blobs"), 0o755); err != nil {
		return nil, err
	}
	if err := b.saveManifest(); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(baseDir, "meta.json"), mustJSON(b.meta), 0o644); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *baseline) captureGit(ctx context.Context, root string) error {
	b.meta.Root = root
	oid, err := gitOut(ctx, root, "stash", "create", "hachi baseline "+b.meta.SessionID)
	if err != nil {
		// stash create fails on an unborn HEAD; everything tracked so
		// far is in the index only, so the empty tree is the baseline
		// and the index files get blob copies below.
		b.meta.BaselineOID = emptyTreeOID
		b.meta.UnbornAtBaseline = true
	} else if oid == "" {
		head, err := gitOut(ctx, root, "rev-parse", "HEAD")
		if err != nil {
			b.meta.BaselineOID = emptyTreeOID
			b.meta.UnbornAtBaseline = true
		} else {
			b.meta.BaselineOID = head
			b.meta.CleanAtBaseline = true
		}
	} else {
		b.meta.BaselineOID = oid
	}
	if head, err := gitOut(ctx, root, "rev-parse", "HEAD"); err == nil {
		b.meta.HeadOID = head
	}
	// Pin the dangling commit so gc cannot prune the baseline out from
	// under a long-lived session. Nothing dangles in the clean and
	// unborn cases.
	if !b.meta.CleanAtBaseline && !b.meta.UnbornAtBaseline {
		if _, err := gitOut(ctx, root, "update-ref", "refs/hachi/baseline/"+b.meta.SessionID, b.meta.BaselineOID); err != nil {
			return fmt.Errorf("engine: pinning baseline: %w", err)
		}
	}
	// Untracked files are invisible to the stash commit; they get
	// manifest entries and, under the caps, content copies.
	paths, err := gitPaths(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	// An unborn repo's staged files are not in any commit either; treat
	// them like untracked so restore has their bytes.
	if b.meta.UnbornAtBaseline {
		staged, err := gitPaths(ctx, root, "ls-files", "-z")
		if err != nil {
			return err
		}
		paths = append(paths, staged...)
	}
	for _, p := range paths {
		if _, err := b.record(p, "untracked"); err != nil {
			return err
		}
	}
	return nil
}

// record adds a manifest entry for path (relative to the baseline root),
// copying its bytes into the blob store when the caps allow. Symlinks
// record their target and are never followed.
func (b *baseline) record(rel, class string) (*ManifestEntry, error) {
	abs := filepath.Join(b.root(), filepath.FromSlash(rel))
	st, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	en := &ManifestEntry{
		Path:  filepath.ToSlash(rel),
		Class: class,
		Size:  st.Size(),
		Mode:  st.Mode(),
		MTime: st.ModTime(),
	}
	if st.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(abs)
		if err != nil {
			return nil, err
		}
		en.LinkTarget = target
		en.Snapshotted = true
	} else if st.Mode().IsRegular() {
		sum, err := hashFile(abs)
		if err != nil {
			return nil, err
		}
		en.SHA256 = sum
		if st.Size() <= maxBlobBytes && b.blobBytes+st.Size() <= maxSessionBytes {
			blob, err := b.writeBlob(abs, sum)
			if err != nil {
				return nil, err
			}
			en.Blob = blob
			en.Snapshotted = true
			b.blobBytes += st.Size()
		}
	}
	b.entries = append(b.entries, en)
	b.byPath[en.Path] = en
	return en, nil
}

// writeBlob copies a file into the content-addressed store, first byte
// of the hash as the shard dir, temp file plus rename so a crash never
// leaves a half-written blob under its final name.
func (b *baseline) writeBlob(abs, sum string) (string, error) {
	rel := filepath.Join("blobs", sum[:2], sum)
	dst := filepath.Join(b.dir, rel)
	if _, err := os.Stat(dst); err == nil {
		return rel, nil // content-addressed: identical bytes cost one copy
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	src, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".blob-*")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return rel, nil
}

func (b *baseline) saveManifest() error {
	return writeFileAtomic(filepath.Join(b.dir, "manifest.json"), mustJSON(b.entries), 0o644)
}

func loadBaseline(dir string) (*baseline, error) {
	mb, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	b := &baseline{dir: dir, byPath: map[string]*ManifestEntry{}}
	if err := json.Unmarshal(mb, &b.meta); err != nil {
		return nil, err
	}
	eb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(eb, &b.entries); err != nil {
		return nil, err
	}
	for _, en := range b.entries {
		b.byPath[en.Path] = en
		if en.Snapshotted && en.Blob != "" {
			b.blobBytes += en.Size
		}
	}
	return b, nil
}

// observeEdit is the pump's hook: it runs for every edit event, before
// the event reaches the journal or any watcher. In a non-git folder the
// started event is the copy-on-write moment, the last chance to save the
// file's pre-session bytes. In both modes the completed event records
// what the agent left behind, which is how review later tells the
// agent's edits from the user's own.
func (e *Engine) observeEdit(id waggle.SessionID, ed waggle.Edit) []waggle.Event {
	e.mu.Lock()
	b, ok := e.bases[id]
	e.mu.Unlock()
	if !ok {
		return nil
	}
	var warns []waggle.Event
	changed := false
	for _, c := range ed.Changes {
		rel, err := relPath(b.root(), c.Path)
		if err != nil {
			continue
		}
		en := b.byPath[rel]
		if en == nil && b.meta.Root == "" {
			// Non-git: first sight of this path. Copy originals now;
			// created files need no bytes, undo just deletes them.
			en, err = b.snapshotNonGit(rel, c.Op, ed.Status)
			if err != nil {
				continue
			}
			changed = true
			if en.Class == "original" && !en.Snapshotted {
				warns = append(warns, waggle.Event{
					Sess: id, Bee: "hachi", Kind: waggle.KindFinding, At: time.Now(),
					Data: waggle.Enc(waggle.Message{Text: fmt.Sprintf(
						"could not save a copy of %s before it was changed, so undo will not cover it", rel)}),
				})
			}
		}
		if ed.Status == "completed" {
			if en == nil {
				// Git mode tracks these through the baseline commit; the
				// manifest only needs a record to hang LastAgentSHA on.
				en = &ManifestEntry{Path: rel, Class: classFor(c.Op)}
				b.entries = append(b.entries, en)
				b.byPath[rel] = en
			}
			abs := filepath.Join(b.root(), filepath.FromSlash(rel))
			if sum, err := hashFile(abs); err == nil {
				en.LastAgentSHA = sum
			} else {
				en.LastAgentSHA = "" // the agent deleted it
			}
			en.LastAgentMTime = time.Now()
			changed = true
		}
	}
	if changed {
		_ = b.saveManifest()
	}
	return warns
}

// snapshotNonGit records a path the agent is about to change in a
// non-git folder. Called at most once per path per session.
func (b *baseline) snapshotNonGit(rel, op, status string) (*ManifestEntry, error) {
	abs := filepath.Join(b.root(), filepath.FromSlash(rel))
	if _, err := os.Lstat(abs); os.IsNotExist(err) {
		// No file yet: a creation we caught in time, or a deletion we
		// heard about too late to save anything.
		en := &ManifestEntry{Path: rel, Class: "created", Snapshotted: true}
		if op == "delete" {
			en.Class, en.Snapshotted = "original", false
		}
		b.entries = append(b.entries, en)
		b.byPath[rel] = en
		return en, nil
	}
	if status != "in_progress" && op != "delete" {
		// The edit already landed; the pre-image is gone and pretending
		// otherwise would make undo restore the agent's own bytes.
		en := &ManifestEntry{Path: rel, Class: "original"}
		b.entries = append(b.entries, en)
		b.byPath[rel] = en
		return en, nil
	}
	return b.record(rel, "original")
}

func classFor(op string) string {
	if op == "add" {
		return "created"
	}
	return "original"
}

// outsideEdited reports whether the file at rel no longer matches what
// the agent last left there, meaning the human changed it after. Only
// meaningful for entries that have a LastAgentMTime.
func (b *baseline) outsideEdited(en *ManifestEntry) bool {
	if en == nil || en.LastAgentMTime.IsZero() {
		return false
	}
	abs := filepath.Join(b.root(), filepath.FromSlash(en.Path))
	sum, err := hashFile(abs)
	if err != nil {
		sum = "" // gone now; agent-deleted reads as empty too
	}
	return sum != en.LastAgentSHA
}

// relPath turns an adapter-reported path into a manifest key relative to
// root. Paths that escape the root are rejected; that is guard's turf,
// not the baseline's.
func relPath(root, p string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("engine: path %s outside %s", p, root)
	}
	return filepath.ToSlash(rel), nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

func mustJSON(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic("engine: unmarshalable baseline record: " + err.Error())
	}
	return b
}

// gitOut runs one git command and returns its trimmed stdout.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// gitPaths runs a git command whose output is NUL-separated paths.
func gitPaths(ctx context.Context, dir string, args ...string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	var paths []string
	for p := range strings.SplitSeq(out.String(), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}
