package codex

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tamnd/hachi/adapter"
	"github.com/tamnd/hachi/waggle"
)

// parseFixture runs the parser over a captured real transcript.
func parseFixture(t *testing.T, name string) []waggle.Event {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	p := parser{sess: "test", bee: "codex"}
	var out []waggle.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		out = append(out, p.line(sc.Bytes())...)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func kinds(evs []waggle.Event) map[waggle.Kind]int {
	m := map[waggle.Kind]int{}
	for _, e := range evs {
		m[e.Kind]++
	}
	return m
}

// TestNoDrift is the drift gate: every line of a real transcript must map
// to a typed event. A KindRaw here means upstream changed shape.
func TestNoDrift(t *testing.T) {
	for _, name := range []string{"simple.jsonl", "tooluse.jsonl"} {
		t.Run(name, func(t *testing.T) {
			for _, ev := range parseFixture(t, name) {
				if ev.Kind == waggle.KindRaw {
					var r waggle.Raw
					_ = json.Unmarshal(ev.Data, &r)
					t.Errorf("unrecognized upstream line: %s", r.Line)
				}
			}
		})
	}
}

func TestSimpleTranscript(t *testing.T) {
	evs := parseFixture(t, "simple.jsonl")
	k := kinds(evs)
	if k[waggle.KindSpawned] != 1 {
		t.Fatalf("want 1 spawned, got %d", k[waggle.KindSpawned])
	}
	if k[waggle.KindMessage] == 0 {
		t.Fatal("no agent message")
	}
	if k[waggle.KindCost] != 1 || k[waggle.KindResult] != 1 {
		t.Fatalf("want 1 cost + 1 result, got %d + %d", k[waggle.KindCost], k[waggle.KindResult])
	}
	if k[waggle.KindDied] != 0 {
		t.Fatal("unexpected died event")
	}

	var sp waggle.Spawned
	if err := json.Unmarshal(evs[0].Data, &sp); err != nil || sp.Resume == "" {
		t.Fatalf("spawned must carry a resume token: %v %+v", err, sp)
	}
	var c waggle.Cost
	for _, ev := range evs {
		if ev.Kind == waggle.KindCost {
			if err := json.Unmarshal(ev.Data, &c); err != nil {
				t.Fatal(err)
			}
		}
	}
	if c.InputTokens == 0 || c.OutputTokens == 0 {
		t.Fatalf("cost should carry real token counts: %+v", c)
	}
}

func TestToolUseTranscript(t *testing.T) {
	evs := parseFixture(t, "tooluse.jsonl")
	var completed []waggle.Tool
	for _, ev := range evs {
		if ev.Kind != waggle.KindTool {
			continue
		}
		var tool waggle.Tool
		if err := json.Unmarshal(ev.Data, &tool); err != nil {
			t.Fatal(err)
		}
		if tool.Status == "completed" {
			completed = append(completed, tool)
		}
	}
	if len(completed) == 0 {
		t.Fatal("tool-use transcript produced no completed tool events")
	}
	for _, tool := range completed {
		if tool.Command == "" {
			t.Errorf("completed tool without a command: %+v", tool)
		}
		if tool.ExitCode == nil {
			t.Errorf("completed tool without exit code: %+v", tool)
		}
	}
}

// TestStreamCloses pins the stream contract on a binary that emits
// nothing: the channel must close and a nonzero exit must surface as died.
func TestStreamCloses(t *testing.T) {
	d := &Driver{Bin: "false"} // exits 1 with no output
	s, err := d.Run(t.Context(), adapter.Session{ID: "t", Dir: t.TempDir()}, "hi")
	if err != nil {
		t.Fatal(err)
	}
	var died bool
	for ev := range s.Events() {
		if ev.Kind == waggle.KindDied {
			died = true
		}
	}
	if !died {
		t.Fatal("nonzero exit with no output must produce a died event")
	}
}
