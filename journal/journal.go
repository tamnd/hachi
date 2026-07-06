// Package journal persists the waggle stream. The default backend is a
// directory of JSONL files, one per session, append-only and line
// buffered so a crash never costs more than the torn last line.
package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/tamnd/hachi/waggle"
)

// Journal stores and replays events.
type Journal interface {
	Append(waggle.Event) error
	Replay(waggle.SessionID) ([]waggle.Event, error)
}

// Meta is the durable per-session record kept next to its events.
type Meta struct {
	ID      waggle.SessionID `json:"id"`
	Title   string           `json:"title"`
	Dir     string           `json:"dir"`
	Brain   string           `json:"brain"`
	Resume  string           `json:"resume,omitempty"`
	Created time.Time        `json:"created"`
	Updated time.Time        `json:"updated"`
}

// Files is the file-backed Journal rooted at a .hive directory.
type Files struct {
	Root string

	mu   sync.Mutex
	open map[waggle.SessionID]*os.File
}

// NewFiles opens (creating if needed) a journal under root.
func NewFiles(root string) (*Files, error) {
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o755); err != nil {
		return nil, err
	}
	return &Files{Root: root, open: map[waggle.SessionID]*os.File{}}, nil
}

func (f *Files) dir(id waggle.SessionID) string {
	return filepath.Join(f.Root, "sessions", string(id))
}

// SessionDir returns the directory a session's files live in. The engine
// keeps the baseline snapshot next to the events it explains.
func (f *Files) SessionDir(id waggle.SessionID) string {
	return f.dir(id)
}

// Append writes one event to its session file.
func (f *Files) Append(e waggle.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.open[e.Sess]
	if !ok {
		if err := os.MkdirAll(f.dir(e.Sess), 0o755); err != nil {
			return err
		}
		var err error
		w, err = os.OpenFile(filepath.Join(f.dir(e.Sess), "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		f.open[e.Sess] = w
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// Replay returns all events for a session in order. A torn final line
// (crash mid-write) is tolerated and dropped.
func (f *Files) Replay(id waggle.SessionID) ([]waggle.Event, error) {
	r, err := os.Open(filepath.Join(f.dir(id), "events.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = r.Close() }()
	var out []waggle.Event
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var e waggle.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// SaveMeta writes the session record atomically.
func (f *Files) SaveMeta(m Meta) error {
	if err := os.MkdirAll(f.dir(m.ID), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(f.dir(m.ID), ".meta.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(f.dir(m.ID), "meta.json"))
}

// LoadMeta reads one session record.
func (f *Files) LoadMeta(id waggle.SessionID) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(filepath.Join(f.dir(id), "meta.json"))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// List returns every session record, newest first.
func (f *Files) List() ([]Meta, error) {
	entries, err := os.ReadDir(filepath.Join(f.Root, "sessions"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Meta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := f.LoadMeta(waggle.SessionID(e.Name()))
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// Close releases open file handles.
func (f *Files) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var first error
	for id, w := range f.open {
		if err := w.Close(); err != nil && first == nil {
			first = fmt.Errorf("journal: closing %s: %w", id, err)
		}
		delete(f.open, id)
	}
	return first
}
