package journal_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tamnd/hachi/journal"
	"github.com/tamnd/hachi/waggle"
)

func TestDeleteRemovesSessionAndHandle(t *testing.T) {
	root := t.TempDir()
	j, err := journal.NewFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()

	id := waggle.SessionID("s1")
	if err := j.SaveMeta(journal.Meta{ID: id, Title: "doomed", Created: time.Now(), Updated: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Append keeps the file handle open; Delete must close it on the way
	// out so nothing writes through a deleted directory entry.
	if err := j.Append(waggle.Event{Sess: id, Seq: 1, Bee: "test", Kind: waggle.KindMessage, At: time.Now()}); err != nil {
		t.Fatal(err)
	}

	if err := j.Delete(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sessions", string(id))); !os.IsNotExist(err) {
		t.Fatalf("the session directory must be gone, err=%v", err)
	}
	metas, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Fatalf("List must not know a deleted session, got %+v", metas)
	}
	// Replay after delete reads as a session that never was.
	evs, err := j.Replay(id)
	if err != nil || len(evs) != 0 {
		t.Fatalf("replay of a deleted session must be empty, got %d events, err=%v", len(evs), err)
	}
	// Deleting again is a quiet no-op: there is nothing left to fail on.
	if err := j.Delete(id); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedSurvivesTheRoundTrip(t *testing.T) {
	j, err := journal.NewFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()

	m := journal.Meta{ID: "s2", Committed: true, Created: time.Now(), Updated: time.Now()}
	if err := j.SaveMeta(m); err != nil {
		t.Fatal(err)
	}
	got, err := j.LoadMeta("s2")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Committed {
		t.Fatal("Committed must survive save and load")
	}
}
