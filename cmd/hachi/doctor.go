package main

// hachi doctor: the explain-and-tidy command. Detect answers "usable or
// not" so startup stays fast; doctor takes the time to say why a brain
// is unusable, and it audits hachi's own leftovers: baseline refs whose
// session is gone, worktrees no session references anymore, branches
// that outlived their session. Plain listing by default, --fix removes
// what is safe to remove. It never touches a branch: branches can hold
// commits, and commits are the user's work.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tamnd/hachi/adapter"
	"github.com/tamnd/hachi/journal"
)

func doctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "explain the install and list hachi's leftovers",
		RunE:  runDoctor,
	}
	cmd.Flags().Bool("fix", false, "remove stale baseline refs and orphaned worktrees")
	return cmd
}

func runDoctor(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	fix, _ := cmd.Flags().GetBool("fix")

	_, _ = fmt.Fprintf(out, "hachi %s\n", version)

	_, _ = fmt.Fprintf(out, "\nbrains\n")
	for _, d := range adapter.Doctor() {
		if d.Err == nil {
			_, _ = fmt.Fprintf(out, "  %-10s ok\n", d.Info.Name)
		} else {
			_, _ = fmt.Fprintf(out, "  %-10s %v\n", d.Info.Name, d.Err)
		}
	}

	home, err := hiveHome()
	if err != nil {
		return err
	}
	j, err := journal.NewFiles(home)
	if err != nil {
		return err
	}
	defer func() { _ = j.Close() }()
	metas, err := j.List()
	if err != nil {
		return err
	}
	word := "sessions"
	if len(metas) == 1 {
		word = "session"
	}
	_, _ = fmt.Fprintf(out, "\nhome\n  %s\n  %d %s\n", home, len(metas), word)

	_, _ = fmt.Fprintf(out, "\nleftovers\n")
	if n := auditLeftovers(out, home, metas, fix); n == 0 {
		_, _ = fmt.Fprintln(out, "  none")
	} else if !fix {
		_, _ = fmt.Fprintln(out, "  hachi doctor --fix removes the refs and worktrees; branches are yours to delete")
	}
	return nil
}

// auditLeftovers walks the three places hachi leaves state and reports
// anything whose session no longer exists. It returns how many findings
// it printed.
func auditLeftovers(out io.Writer, home string, metas []journal.Meta, fix bool) int {
	sessions := map[string]bool{}
	wtInUse := map[string]bool{}
	branchInUse := map[string]bool{}
	roots := map[string]bool{}
	for _, m := range metas {
		sessions[string(m.ID)] = true
		if m.WorktreePath != "" {
			wtInUse[canon(m.WorktreePath)] = true
			if r, ok := checkoutRoot(m.WorktreePath); ok {
				roots[r] = true
			}
		} else if r, err := gitIn(m.Dir, "rev-parse", "--show-toplevel"); err == nil && r != "" {
			roots[canon(r)] = true
		}
		if m.WorktreeBranch != "" {
			branchInUse[m.WorktreeBranch] = true
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		if r, err := gitIn(cwd, "rev-parse", "--show-toplevel"); err == nil && r != "" {
			roots[canon(r)] = true
		}
	}

	found := 0

	// Worktrees live under the hive home, one folder per repo; anything
	// there that no session meta points at is an orphan (a crash, or a
	// journal deleted by hand).
	repoDirs, _ := os.ReadDir(filepath.Join(home, "worktrees"))
	for _, rd := range repoDirs {
		if !rd.IsDir() {
			continue
		}
		wts, _ := os.ReadDir(filepath.Join(home, "worktrees", rd.Name()))
		for _, wt := range wts {
			if !wt.IsDir() {
				continue
			}
			path := filepath.Join(home, "worktrees", rd.Name(), wt.Name())
			if wtInUse[canon(path)] {
				continue
			}
			found++
			root, ok := checkoutRoot(path)
			if ok {
				roots[root] = true
			}
			if fix && ok {
				// No --force: a worktree with uncommitted changes stays,
				// because doctor does not destroy work.
				if _, err := gitIn(root, "worktree", "remove", path); err != nil {
					_, _ = fmt.Fprintf(out, "  orphaned worktree %s kept: it has unsaved changes; git worktree remove --force removes it anyway\n", path)
				} else {
					_, _ = fmt.Fprintf(out, "  removed orphaned worktree %s\n", path)
				}
			} else {
				_, _ = fmt.Fprintf(out, "  orphaned worktree %s: no session references it\n", path)
			}
		}
	}

	// Baseline refs pin snapshot objects against gc; one per session, so
	// a ref without its session directory keeps dead objects alive.
	for _, root := range sorted(roots) {
		refs, err := gitIn(root, "for-each-ref", "--format=%(refname)", "refs/hachi/")
		if err != nil || refs == "" {
			continue
		}
		for _, ref := range strings.Split(refs, "\n") {
			sid := ref[strings.LastIndex(ref, "/")+1:]
			if sid == "" || sessions[sid] {
				continue
			}
			found++
			if fix {
				if _, err := gitIn(root, "update-ref", "-d", ref); err == nil {
					_, _ = fmt.Fprintf(out, "  removed stale ref %s in %s\n", ref, root)
				} else {
					_, _ = fmt.Fprintf(out, "  stale ref %s in %s could not be removed: %v\n", ref, root, err)
				}
			} else {
				_, _ = fmt.Fprintf(out, "  stale ref %s in %s: session %s is gone\n", ref, root, sid)
			}
		}
	}

	// Branches that outlived their session hold commits nothing else
	// reaches; doctor names them and stops there.
	for _, root := range sorted(roots) {
		brs, err := gitIn(root, "for-each-ref", "--format=%(refname:short)", "refs/heads/hachi/")
		if err != nil || brs == "" {
			continue
		}
		for _, br := range strings.Split(brs, "\n") {
			if br == "" || branchInUse[br] {
				continue
			}
			found++
			_, _ = fmt.Fprintf(out, "  branch %s in %s has no session; git branch -d deletes it once its commits are merged\n", br, root)
		}
	}

	return found
}

// checkoutRoot finds the user's checkout a worktree hangs off: the
// shared .git directory's parent.
func checkoutRoot(worktree string) (string, bool) {
	common, err := gitIn(worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", false
	}
	return canon(filepath.Dir(common)), true
}

// canon resolves symlinks so two spellings of one path compare equal;
// macOS spells temp dirs both /var and /private/var.
func canon(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// gitIn runs one git command in dir and returns trimmed stdout.
func gitIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
