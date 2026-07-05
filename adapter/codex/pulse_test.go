package codex

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tamnd/hachi/waggle"
)

// The literals below mirror the shapes codex writes to its rollout log,
// captured from real sessions.

func tokenCountLine(in, cached, out, reasoning int64) string {
	itoa := func(n int64) string { return strconv.FormatInt(n, 10) }
	return `{"timestamp":"2026-07-06T00:00:00.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0,"reasoning_output_tokens":0,"total_tokens":0},"last_token_usage":{"input_tokens":` +
		itoa(in) + `,"cached_input_tokens":` + itoa(cached) + `,"output_tokens":` + itoa(out) +
		`,"reasoning_output_tokens":` + itoa(reasoning) + `,"total_tokens":0},"model_context_window":258400},"rate_limits":{"limit_id":"codex","limit_name":null,"primary":{"used_percent":1.0,"window_minutes":300,"resets_at":1783300371},"secondary":{"used_percent":15.0,"window_minutes":10080,"resets_at":1783845194},"credits":null,"plan_type":"plus"}}}` + "\n"
}

func turnContextLine(model, effort string) string {
	return `{"timestamp":"2026-07-06T00:00:00.000Z","type":"turn_context","payload":{"cwd":"/tmp","approval_policy":"never","model":"` +
		model + `","effort":"` + effort + `","summary":"auto"}}` + "\n"
}

func TestTailRollout(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2026", "07", "06")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-07-06T00-00-00-threadx.jsonl")
	seed := `{"type":"session_meta","payload":{}}` + "\n" +
		turnContextLine("gpt-5-codex", "low") +
		`{"type":"event_msg","payload":{"type":"task_started"}}` + "\n" +
		tokenCountLine(100, 40, 7, 2)
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got := make(chan waggle.Pulse, 16)
	go tailRollout(ctx, root, "threadx", true, func(p waggle.Pulse) { got <- p })

	first := recvPulse(t, got)
	u := first.Usage
	if u.InputTokens != 100 || u.CachedInputTokens != 40 || u.OutputTokens != 7 || u.ReasoningTokens != 2 {
		t.Fatalf("first pulse usage wrong: %+v", u)
	}
	if first.Model != "gpt-5-codex" || first.Effort != "low" {
		t.Fatalf("pulse missed the turn context: %+v", first)
	}
	if first.Window != 258400 || first.Context != 107 {
		t.Fatalf("pulse missed the context window: window=%d context=%d", first.Window, first.Context)
	}
	if len(first.Limits) != 2 || first.Limits[0].Name != "5h" || first.Limits[0].UsedPct != 1.0 ||
		first.Limits[1].Name != "week" || first.Limits[1].UsedPct != 15.0 {
		t.Fatalf("pulse missed the rate limits: %+v", first.Limits)
	}
	if first.Limits[0].ResetsAt.Unix() != 1783300371 {
		t.Fatalf("reset time wrong: %v", first.Limits[0].ResetsAt)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(tokenCountLine(50, 0, 5, 0)); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	second := recvPulse(t, got)
	if second.Usage.InputTokens != 150 || second.Usage.OutputTokens != 12 {
		t.Fatalf("usage must accumulate within the turn: %+v", second.Usage)
	}
	if second.Context != 55 {
		t.Fatalf("context must track the latest call, not accumulate: %d", second.Context)
	}
}

// TestTailRolloutResumeSkipsHistory pins the resume behavior: token counts
// already in the log belong to earlier turns and must not be recounted.
func TestTailRolloutResumeSkipsHistory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2026", "07", "06")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-07-06T00-00-01-thready.jsonl")
	if err := os.WriteFile(path, []byte(tokenCountLine(9999, 0, 9999, 0)), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got := make(chan waggle.Pulse, 16)
	go tailRollout(ctx, root, "thready", false, func(p waggle.Pulse) { got <- p })

	// Give the tailer time to open the file and settle past the history.
	time.Sleep(700 * time.Millisecond)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(tokenCountLine(10, 0, 3, 0)); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	p := recvPulse(t, got)
	if p.Usage.InputTokens != 10 || p.Usage.OutputTokens != 3 {
		t.Fatalf("resume tail must start after existing history: %+v", p.Usage)
	}
}

func TestLimitName(t *testing.T) {
	cases := map[int64]string{300: "5h", 10080: "week", 60: "1h", 1440: "1d", 90: "90m", 0: "limit"}
	for mins, want := range cases {
		if got := limitName(mins); got != want {
			t.Errorf("limitName(%d) = %q, want %q", mins, got, want)
		}
	}
}

func recvPulse(t *testing.T, ch <-chan waggle.Pulse) waggle.Pulse {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(8 * time.Second):
		t.Fatal("no pulse arrived")
		return waggle.Pulse{}
	}
}
