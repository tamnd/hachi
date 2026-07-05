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

// tokenCountLine mirrors the shape codex writes to its rollout log; the
// literal below was captured from a real session.
func tokenCountLine(in, cached, out, reasoning int64) string {
	itoa := func(n int64) string { return strconv.FormatInt(n, 10) }
	return `{"timestamp":"2026-07-06T00:00:00.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0,"reasoning_output_tokens":0,"total_tokens":0},"last_token_usage":{"input_tokens":` +
		itoa(in) + `,"cached_input_tokens":` + itoa(cached) + `,"output_tokens":` + itoa(out) +
		`,"reasoning_output_tokens":` + itoa(reasoning) + `,"total_tokens":0}}}}` + "\n"
}

func TestTailTokens(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2026", "07", "06")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-07-06T00-00-00-threadx.jsonl")
	seed := `{"type":"session_meta","payload":{}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"task_started"}}` + "\n" +
		tokenCountLine(100, 40, 7, 2)
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got := make(chan waggle.Cost, 16)
	go tailTokens(ctx, root, "threadx", true, func(c waggle.Cost) { got <- c })

	first := recvCost(t, got)
	if !first.Live || first.InputTokens != 100 || first.CachedInputTokens != 40 || first.OutputTokens != 7 || first.ReasoningTokens != 2 {
		t.Fatalf("first pulse wrong: %+v", first)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(tokenCountLine(50, 0, 5, 0)); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	second := recvCost(t, got)
	if second.InputTokens != 150 || second.OutputTokens != 12 {
		t.Fatalf("pulses must accumulate within the turn: %+v", second)
	}
}

// TestTailTokensResumeSkipsHistory pins the resume behavior: token counts
// already in the log belong to earlier turns and must not be recounted.
func TestTailTokensResumeSkipsHistory(t *testing.T) {
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
	got := make(chan waggle.Cost, 16)
	go tailTokens(ctx, root, "thready", false, func(c waggle.Cost) { got <- c })

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

	c := recvCost(t, got)
	if c.InputTokens != 10 || c.OutputTokens != 3 {
		t.Fatalf("resume tail must start after existing history: %+v", c)
	}
}

func recvCost(t *testing.T, ch <-chan waggle.Cost) waggle.Cost {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(8 * time.Second):
		t.Fatal("no pulse arrived")
		return waggle.Cost{}
	}
}
