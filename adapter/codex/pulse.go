package codex

// Live token pulse. codex exec --json reports usage only at turn end, but
// the CLI appends a token_count line to its rollout log after every model
// call. Tailing that log is what lets a client show tokens arriving while
// the turn runs. Everything here is best effort: no rollout file means no
// pulses, never a failed turn.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/tamnd/hachi/waggle"
)

// sessionsRoot is the directory codex writes rollout logs under.
func (d *Driver) sessionsRoot() string {
	if d.Sessions != "" {
		return d.Sessions
	}
	root := os.Getenv("CODEX_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		root = filepath.Join(home, ".codex")
	}
	return filepath.Join(root, "sessions")
}

// findRollout locates the rollout log for a thread. Fresh threads get the
// file created moments after thread.started, so poll briefly.
func findRollout(ctx context.Context, root, thread string) string {
	if root == "" || thread == "" {
		return ""
	}
	pattern := filepath.Join(root, "*", "*", "*", "rollout-*-"+thread+".jsonl")
	for range 40 {
		if m, _ := filepath.Glob(pattern); len(m) > 0 {
			return m[0]
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(250 * time.Millisecond):
		}
	}
	return ""
}

type rolloutLine struct {
	Type    string `json:"type"`
	Payload struct {
		Type string `json:"type"`
		Info struct {
			Last struct {
				Input     int64 `json:"input_tokens"`
				Cached    int64 `json:"cached_input_tokens"`
				Output    int64 `json:"output_tokens"`
				Reasoning int64 `json:"reasoning_output_tokens"`
			} `json:"last_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

// tailTokens follows the rollout log and calls emit with the turn's usage
// so far each time codex records another model call. A fresh thread reads
// the file from the start (everything in it belongs to this turn); a
// resumed one starts at the current end so old turns are not recounted.
// Returns when ctx is canceled.
func tailTokens(ctx context.Context, root, thread string, fromStart bool, emit func(waggle.Cost)) {
	path := findRollout(ctx, root, thread)
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	var off int64
	if !fromStart {
		if st, err := f.Stat(); err == nil {
			off = st.Size()
		}
	}

	var sum waggle.Cost
	var pending []byte
	chunk := make([]byte, 256*1024)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
		n, _ := f.ReadAt(chunk, off)
		if n <= 0 {
			continue
		}
		off += int64(n)
		pending = append(pending, chunk[:n]...)
		for {
			i := bytes.IndexByte(pending, '\n')
			if i < 0 {
				break
			}
			line := pending[:i]
			pending = pending[i+1:]
			var rl rolloutLine
			if json.Unmarshal(line, &rl) != nil {
				continue
			}
			if rl.Type != "event_msg" || rl.Payload.Type != "token_count" {
				continue
			}
			u := rl.Payload.Info.Last
			if u.Input+u.Cached+u.Output+u.Reasoning == 0 {
				continue
			}
			sum.InputTokens += u.Input
			sum.CachedInputTokens += u.Cached
			sum.OutputTokens += u.Output
			sum.ReasoningTokens += u.Reasoning
			snap := sum
			snap.Live = true
			emit(snap)
		}
	}
}
