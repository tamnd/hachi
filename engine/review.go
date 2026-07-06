package engine

// Accepting work. Stage is git add scoped to what the session changed,
// CommitDraft fills a message template from the session's own record,
// and Commit runs git commit with whatever the human left in the
// editor. The rule the whole flow hangs on: hachi never commits without
// the message passing through an editor the user saw. There is no
// bypass flag, so that rule cannot erode one convenience at a time.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

// Stage implements hive.Service. Nil paths means the whole change set
// minus files flagged as changed outside the session; naming a path is
// the confirmation that stages even a flagged one. Deletions stage like
// anything else. Returns the paths actually staged.
func (e *Engine) Stage(ctx context.Context, id waggle.SessionID, paths []string) ([]string, error) {
	b, err := e.ensureBaseline(ctx, id)
	if err != nil {
		return nil, err
	}
	diffs, err := e.Changes(ctx, id)
	if err != nil {
		return nil, err
	}
	inSet := map[string]bool{}
	var picked []string
	if paths == nil {
		for _, d := range diffs {
			if !d.Outside {
				picked = append(picked, d.Path)
			}
		}
	} else {
		for _, d := range diffs {
			inSet[d.Path] = true
		}
		for _, p := range paths {
			// Only what this session changed; git add on anything else
			// would sweep the user's own dirty files into the commit.
			if inSet[p] {
				picked = append(picked, p)
			}
		}
	}
	if len(picked) == 0 {
		return nil, nil
	}
	if b.meta.Root != "" {
		// Narrow to paths the index does not already hold. git add on an
		// already staged deletion fails hard ("pathspec did not match any
		// files"), and staging twice has to be a quiet no-op.
		need, err := needsAdd(ctx, b.meta.Root, picked)
		if err != nil {
			return nil, err
		}
		if len(need) > 0 {
			args := append([]string{"add", "--"}, need...)
			if _, err := gitOut(ctx, b.meta.Root, args...); err != nil {
				return nil, err
			}
		}
	}
	for _, p := range picked {
		en := b.byPath[p]
		if en == nil {
			en = &ManifestEntry{Path: p, Class: "original"}
			b.entries = append(b.entries, en)
			b.byPath[p] = en
		}
		en.Staged = true
	}
	_ = b.saveManifest()
	return picked, nil
}

// needsAdd returns the picked paths whose working tree still differs
// from the index, read from git status itself. A path git already holds
// staged (second column blank) has nothing left to add.
func needsAdd(ctx context.Context, root string, picked []string) ([]string, error) {
	args := append([]string{"status", "--porcelain=v1", "-z", "--no-renames", "--"}, picked...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	// Raw stdout on purpose: a record whose first column is blank starts
	// with a space, and trimming would shift every column read below.
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git status: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	var need []string
	for entry := range strings.SplitSeq(out.String(), "\x00") {
		if len(entry) < 4 {
			continue
		}
		if entry[1] != ' ' {
			need = append(need, entry[3:])
		}
	}
	return need, nil
}

// CommitDraft implements hive.Service: a message built from the ask and
// the change set, deterministic, never a model call. It exists to be
// edited, which is why it aims for plain over clever.
func (e *Engine) CommitDraft(ctx context.Context, id waggle.SessionID) (string, error) {
	b, err := e.ensureBaseline(ctx, id)
	if err != nil {
		return "", err
	}
	if b.meta.Root == "" {
		return "", fmt.Errorf("engine: %s is not inside a git repository", b.meta.Dir)
	}
	m, err := e.Journal.LoadMeta(id)
	if err != nil {
		return "", err
	}
	diffs, err := e.Changes(ctx, id)
	if err != nil {
		return "", err
	}
	title := draftTitle(m.Title, diffs)
	body := draftBody(diffs)
	if body == "" {
		return title + "\n", nil
	}
	return title + "\n\n" + body + "\n", nil
}

// draftBody says what changed in plain sentences, grouped by kind, no
// bullets. "Adds a. Updates b and c. Deletes d."
func draftBody(diffs []hive.FileDiff) string {
	var added, updated, deleted []string
	for _, d := range diffs {
		switch d.Status {
		case "A":
			added = append(added, d.Path)
		case "D":
			deleted = append(deleted, d.Path)
		default:
			updated = append(updated, d.Path)
		}
	}
	var sentences []string
	for _, g := range []struct {
		verb  string
		paths []string
	}{{"Adds", added}, {"Updates", updated}, {"Deletes", deleted}} {
		if len(g.paths) > 0 {
			sentences = append(sentences, g.verb+" "+joinAnd(g.paths)+".")
		}
	}
	return strings.Join(sentences, " ")
}

// joinAnd writes a list the way a person would: "a", "a and b",
// "a, b, and c".
func joinAnd(items []string) string {
	switch len(items) {
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
}

// draftTitle turns the session's opening ask into a commit title:
// first line, capitalized, no trailing period, cut at a word under 60
// characters. An unusable ask falls back to naming the files.
func draftTitle(ask string, diffs []hive.FileDiff) string {
	t := strings.TrimSpace(strings.SplitN(ask, "\n", 2)[0])
	t = strings.TrimRight(t, ".!")
	if len(t) > 60 {
		cut := strings.LastIndex(t[:60], " ")
		if cut < 20 {
			cut = 60
		}
		t = strings.TrimRight(t[:cut], " ,;:")
	}
	if t == "" {
		switch len(diffs) {
		case 0:
			return "Update files"
		case 1:
			return "Update " + diffs[0].Path
		default:
			return fmt.Sprintf("Update %s and %d more", diffs[0].Path, len(diffs)-1)
		}
	}
	r := []rune(t)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// Commit implements hive.Service: git commit with the message exactly
// as the human left it, scoped to the paths staged through hachi. The
// scoping matters: a plain commit would take the whole index, sweeping
// in whatever the user had staged themselves before the session. The
// repo's own identity, hooks, and signing apply untouched, and their
// output comes back verbatim, success or not.
func (e *Engine) Commit(ctx context.Context, id waggle.SessionID, message string) (string, error) {
	b, err := e.ensureBaseline(ctx, id)
	if err != nil {
		return "", err
	}
	if b.meta.Root == "" {
		return "", fmt.Errorf("engine: %s is not inside a git repository", b.meta.Dir)
	}
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("engine: refusing an empty commit message")
	}
	var staged []string
	for _, en := range b.entries {
		if en.Staged {
			staged = append(staged, en.Path)
		}
	}
	if len(staged) == 0 {
		return "", fmt.Errorf("engine: nothing staged through hachi yet")
	}
	tmp, err := os.CreateTemp("", "hachi-commit-*.txt")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(message); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	args := append([]string{"commit", "--only", "-F", tmp.Name(), "--"}, staged...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = b.meta.Root
	// One buffer for both streams so hook chatter reads in the order the
	// hook printed it.
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	runErr := cmd.Run()
	output := strings.TrimSpace(out.String())
	if runErr != nil {
		return output, fmt.Errorf("git commit: %w", runErr)
	}

	sha, _ := gitOut(ctx, b.meta.Root, "rev-parse", "--short", "HEAD")
	first := strings.TrimSpace(strings.SplitN(message, "\n", 2)[0])
	e.ensureSeq(id)
	e.append(waggle.Event{Sess: id, Bee: "hachi", Kind: waggle.KindFinding, At: time.Now(),
		Data: waggle.Enc(waggle.Message{Text: fmt.Sprintf("committed %s: %s", sha, first)})})
	return output, nil
}
