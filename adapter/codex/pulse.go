package codex

// Rollout vitals. codex exec --json reports usage only at turn end, but
// the CLI keeps a rollout log under ~/.codex/sessions and appends to it
// after every model call: token counts, how full the context window is,
// the subscription's rate-limit meters, and the model and effort the turn
// runs with. Tailing that log is what lets a client show live vitals
// while the turn works. Everything here is best effort: no rollout file
// means no pulses, never a failed turn.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// rolloutLine covers the rollout lines the tail cares about: turn_context
// rows carry the model and effort, token_count rows carry usage, the
// context window, and rate limits, patch_apply_end rows carry the actual
// file contents behind an edit.
type rolloutLine struct {
	Type    string `json:"type"`
	Payload struct {
		Type    string `json:"type"`
		Model   string `json:"model"`
		Effort  string `json:"effort"`
		Success *bool  `json:"success"`
		Changes map[string]struct {
			Type        string `json:"type"`
			Content     string `json:"content"`
			UnifiedDiff string `json:"unified_diff"`
		} `json:"changes"`
		Info struct {
			Window int64 `json:"model_context_window"`
			Last   struct {
				Input     int64 `json:"input_tokens"`
				Cached    int64 `json:"cached_input_tokens"`
				Output    int64 `json:"output_tokens"`
				Reasoning int64 `json:"reasoning_output_tokens"`
			} `json:"last_token_usage"`
		} `json:"info"`
		RateLimits struct {
			Primary   *rolloutWindow `json:"primary"`
			Secondary *rolloutWindow `json:"secondary"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

type rolloutWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int64   `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

// tailRollout follows the rollout log, calling emit with the brain's
// vitals each time codex records more and onPatch with the diffs behind
// each applied patch. A fresh thread reads the file from the start
// (everything in it belongs to this turn); a resumed one starts at the
// current end so old turns are not recounted. Returns when ctx is
// canceled.
func tailRollout(ctx context.Context, root, thread string, fromStart bool, emit func(waggle.Pulse), onPatch func(map[string]string)) {
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

	var p waggle.Pulse
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
		changed := false
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
			if fold(&p, rl) {
				changed = true
			}
			if d := patchDiffs(rl); len(d) > 0 && onPatch != nil {
				onPatch(d)
			}
		}
		if changed {
			emit(p)
		}
	}
}

// fold merges one rollout line into the vitals and reports whether it
// carried anything new.
func fold(p *waggle.Pulse, rl rolloutLine) bool {
	switch {
	case rl.Type == "turn_context":
		if rl.Payload.Model == "" {
			return false
		}
		if rl.Payload.Model == p.Model && rl.Payload.Effort == p.Effort {
			return false
		}
		p.Model, p.Effort = rl.Payload.Model, rl.Payload.Effort
		return true

	case rl.Type == "event_msg" && rl.Payload.Type == "token_count":
		changed := false
		if w := rl.Payload.Info.Window; w > 0 && w != p.Window {
			p.Window = w
			changed = true
		}
		u := rl.Payload.Info.Last
		if u.Input+u.Cached+u.Output+u.Reasoning > 0 {
			p.Usage.InputTokens += u.Input
			p.Usage.CachedInputTokens += u.Cached
			p.Usage.OutputTokens += u.Output
			p.Usage.ReasoningTokens += u.Reasoning
			// The latest call's input plus output is what sits in the
			// window for the next call.
			p.Context = u.Input + u.Output
			changed = true
		}
		var lims []waggle.Limit
		for _, w := range []*rolloutWindow{rl.Payload.RateLimits.Primary, rl.Payload.RateLimits.Secondary} {
			if w == nil {
				continue
			}
			l := waggle.Limit{Name: limitName(w.WindowMinutes), UsedPct: w.UsedPercent}
			if w.ResetsAt > 0 {
				l.ResetsAt = time.Unix(w.ResetsAt, 0)
			}
			lims = append(lims, l)
		}
		if len(lims) > 0 {
			p.Limits = lims
			changed = true
		}
		return changed
	}
	return false
}

// patchDiffs extracts display diffs from a patch_apply_end line, keyed by
// path. Failed patches carry nothing worth showing.
func patchDiffs(rl rolloutLine) map[string]string {
	if rl.Type != "event_msg" || rl.Payload.Type != "patch_apply_end" || len(rl.Payload.Changes) == 0 {
		return nil
	}
	if rl.Payload.Success != nil && !*rl.Payload.Success {
		return nil
	}
	out := map[string]string{}
	for path, c := range rl.Payload.Changes {
		if d := patchDiff(c.Type, c.Content, c.UnifiedDiff); d != "" {
			out[path] = d
		}
	}
	return out
}

// limitName turns a rate-limit window length into the label a human would
// use for it: codex's 300 minute window reads "5h", 10080 reads "week".
func limitName(mins int64) string {
	switch {
	case mins <= 0:
		return "limit"
	case mins == 10080:
		return "week"
	case mins%1440 == 0:
		return fmt.Sprintf("%dd", mins/1440)
	case mins%60 == 0:
		return fmt.Sprintf("%dh", mins/60)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}
