package codex

import (
	"reflect"
	"testing"
)

// TestRunArgs pins the invocation shape that keeps the provider's prompt
// cache warm: follow-up turns resume the stored thread, and every flag
// outside resume and the message is identical between turns. Changing the
// profile mid-conversation would invalidate the cached prefix and re-bill
// the history.
func TestRunArgs(t *testing.T) {
	d := &Driver{Bin: "codex"}

	fresh := d.runArgs("", "hello")
	wantFresh := []string{
		"exec",
		"--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=true",
		"--json", "--skip-git-repo-check",
		"hello",
	}
	if !reflect.DeepEqual(fresh, wantFresh) {
		t.Fatalf("fresh turn args:\n got %q\nwant %q", fresh, wantFresh)
	}

	followup := d.runArgs("thread-1", "again")
	wantFollowup := []string{
		"exec",
		"--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=true",
		"resume", "thread-1",
		"--json", "--skip-git-repo-check",
		"again",
	}
	if !reflect.DeepEqual(followup, wantFollowup) {
		t.Fatalf("follow-up turn args:\n got %q\nwant %q", followup, wantFollowup)
	}
}
