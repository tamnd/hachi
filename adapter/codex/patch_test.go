package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tamnd/hachi/waggle"
)

// TestPatchMergeDiffAfterCard covers the common order: exec --json emits
// the edit card first, the rollout diff lands moments later and must come
// back under the same ref.
func TestPatchMergeDiffAfterCard(t *testing.T) {
	m := newPatchMerge()
	e := waggle.Edit{Ref: "item_2", Status: "completed",
		Changes: []waggle.FileChange{{Path: "/w/a.go", Op: "update"}}}
	m.fold(&e)
	if e.Changes[0].Diff != "" {
		t.Fatalf("no diff should be attached yet: %+v", e.Changes[0])
	}

	out := m.apply(map[string]string{"/w/a.go": "@@ -1 +1 @@\n-old\n+new"})
	if len(out) != 1 || out[0].Ref != "item_2" {
		t.Fatalf("diff must re-emit the original card: %+v", out)
	}
	if out[0].Changes[0].Diff != "@@ -1 +1 @@\n-old\n+new" {
		t.Fatalf("diff not attached: %+v", out[0].Changes[0])
	}
}

// TestPatchMergeDiffBeforeCard covers the tail winning the race: a diff
// with no card yet waits until fold sees the path.
func TestPatchMergeDiffBeforeCard(t *testing.T) {
	m := newPatchMerge()
	if out := m.apply(map[string]string{"/w/b.sh": "+echo hi"}); len(out) != 0 {
		t.Fatalf("nothing to re-emit before the card exists: %+v", out)
	}
	e := waggle.Edit{Ref: "item_5", Status: "completed",
		Changes: []waggle.FileChange{{Path: "/w/b.sh", Op: "add"}}}
	m.fold(&e)
	if e.Changes[0].Diff != "+echo hi" {
		t.Fatalf("pending diff must attach on fold: %+v", e.Changes[0])
	}
}

// TestPatchDiffs checks parsing of a real-shaped patch_apply_end line:
// updates keep their unified diff, adds become all-plus lines, and failed
// patches produce nothing.
func TestPatchDiffs(t *testing.T) {
	line := `{"type":"event_msg","payload":{"type":"patch_apply_end","call_id":"call_1","stdout":"ok","success":true,` +
		`"changes":{` +
		`"/w/hello.sh":{"type":"add","content":"#!/bin/sh\necho HONEY_OK\n"},` +
		`"/w/main.go":{"type":"update","unified_diff":"@@ -2 +2,2 @@\n old\n+new\n","move_path":null}` +
		`},"status":"completed"}}`
	var rl rolloutLine
	if err := json.Unmarshal([]byte(line), &rl); err != nil {
		t.Fatal(err)
	}
	d := patchDiffs(rl)
	if len(d) != 2 {
		t.Fatalf("want 2 diffs, got %v", d)
	}
	if d["/w/hello.sh"] != "+#!/bin/sh\n+echo HONEY_OK" {
		t.Fatalf("add content must become plus lines: %q", d["/w/hello.sh"])
	}
	if !strings.HasPrefix(d["/w/main.go"], "@@ -2 +2,2 @@") {
		t.Fatalf("update must keep its unified diff: %q", d["/w/main.go"])
	}

	failed := strings.Replace(line, `"success":true`, `"success":false`, 1)
	if err := json.Unmarshal([]byte(failed), &rl); err != nil {
		t.Fatal(err)
	}
	if d := patchDiffs(rl); d != nil {
		t.Fatalf("failed patches carry nothing to show: %v", d)
	}
}
