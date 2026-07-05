package codex

// codex reports file changes in two places: exec --json says which paths
// changed, and the rollout log's patch_apply_end carries the actual
// contents. patchMerge joins them. Whichever side arrives first waits for
// the other, and the enriched edit goes out under the original card's
// ref, so clients that update in place get the diff filled in moments
// after the card appears.

import (
	"strings"
	"sync"

	"github.com/tamnd/hachi/waggle"
)

type patchMerge struct {
	mu      sync.Mutex
	edits   map[string]waggle.Edit // ref -> last emitted payload
	refs    map[string]string      // path -> ref of the card showing it
	pending map[string]string      // path -> diff that arrived before its card
}

func newPatchMerge() *patchMerge {
	return &patchMerge{
		edits:   map[string]waggle.Edit{},
		refs:    map[string]string{},
		pending: map[string]string{},
	}
}

// fold runs on every edit the exec parser builds: attach any diff that
// already arrived and remember the card so later diffs find it.
func (m *patchMerge) fold(e *waggle.Edit) {
	if m == nil || e.Ref == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range e.Changes {
		if d, ok := m.pending[c.Path]; ok && c.Diff == "" {
			e.Changes[i].Diff = d
			delete(m.pending, c.Path)
		}
		m.refs[c.Path] = e.Ref
	}
	m.edits[e.Ref] = snapshot(*e)
}

// apply takes the diffs from one patch_apply_end and returns the edits
// that must be re-emitted with diffs attached. Diffs with no card yet are
// held until fold sees the path.
func (m *patchMerge) apply(diffs map[string]string) []waggle.Edit {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	touched := map[string]bool{}
	for path, diff := range diffs {
		ref, ok := m.refs[path]
		if !ok {
			m.pending[path] = diff
			continue
		}
		e := m.edits[ref]
		for i, c := range e.Changes {
			if c.Path == path {
				e.Changes[i].Diff = diff
			}
		}
		m.edits[ref] = e
		touched[ref] = true
	}
	out := make([]waggle.Edit, 0, len(touched))
	for ref := range touched {
		out = append(out, snapshot(m.edits[ref]))
	}
	return out
}

// snapshot copies an edit deeply enough that callers and the merge never
// share a Changes slice.
func snapshot(e waggle.Edit) waggle.Edit {
	e.Changes = append([]waggle.FileChange(nil), e.Changes...)
	return e
}

// patchDiff turns one rollout change entry into display lines. Updates
// come with a unified diff already; adds and deletes only carry contents,
// so they become all-plus or all-minus lines.
func patchDiff(kind, content, unified string) string {
	if unified != "" {
		return strings.TrimRight(unified, "\n")
	}
	if content == "" {
		return ""
	}
	mark := "+"
	if kind == "delete" {
		mark = "-"
	}
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	for i, l := range lines {
		lines[i] = mark + l
	}
	return strings.Join(lines, "\n")
}
