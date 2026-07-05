// Package codex is the first brain driver: it wraps the codex CLI in
// non-interactive mode. One process per turn, JSONL on stdout, resume by
// thread id. The parser is small and owned here on purpose; upstream
// shapes were captured from real codex-cli runs and live in testdata.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"syscall"
	"time"

	"github.com/tamnd/hachi/adapter"
	"github.com/tamnd/hachi/waggle"
)

const name = "codex"

func init() {
	adapter.Register(adapter.Info{Name: name, Steer: adapter.SteerResume}, func() (adapter.Adapter, error) {
		return &Driver{Bin: "codex"}, nil
	})
}

// Driver runs codex exec. Bin is overridable for tests and rigs that pin
// a specific binary or wrap it with a different config profile.
type Driver struct {
	Bin string
	// Args are extra arguments appended before the prompt, letting eval
	// rigs point at a local model profile without the driver knowing.
	Args []string
}

// Detect reports whether the codex binary is on PATH.
func (d *Driver) Detect() error {
	_, err := exec.LookPath(d.Bin)
	return err
}

// Run starts one turn: a fresh thread when sess.Resume is empty, otherwise
// codex exec resume with the stored thread id.
func (d *Driver) Run(ctx context.Context, sess adapter.Session, msg string) (adapter.Stream, error) {
	args := []string{"exec", "--json", "--skip-git-repo-check"}
	if sess.Resume != "" {
		args = []string{"exec", "resume", sess.Resume, "--json", "--skip-git-repo-check"}
	}
	args = append(args, d.Args...)
	args = append(args, msg)

	cmd := exec.CommandContext(ctx, d.Bin, args...)
	cmd.Dir = sess.Dir
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil // progress noise; the JSONL on stdout is the record
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	s := &stream{ch: make(chan waggle.Event, 64), cancel: func() { _ = cmd.Process.Signal(syscall.SIGTERM) }}
	go func() {
		defer close(s.ch)
		p := parser{sess: sess.ID, bee: name}
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		done := false
		for !done && sc.Scan() {
			for _, ev := range p.line(sc.Bytes()) {
				s.ch <- ev
				// The turn is over at its terminal event. The process can
				// linger after turn.completed (telemetry flush, child pipes),
				// so stop reading here and reap it instead of waiting.
				if ev.Kind == waggle.KindResult || ev.Kind == waggle.KindDied {
					done = true
				}
			}
		}
		if done {
			// Close the stream now; reap the process off to the side so a
			// slow shutdown never holds the turn open.
			go func() {
				_ = cmd.Process.Signal(syscall.SIGTERM)
				kill := time.AfterFunc(5*time.Second, func() { _ = cmd.Process.Kill() })
				_, _ = io.Copy(io.Discard, stdout)
				_ = cmd.Wait()
				kill.Stop()
			}()
			return
		}
		err := cmd.Wait()
		if err != nil && ctx.Err() == nil {
			var xe *exec.ExitError
			code := -1
			if errors.As(err, &xe) {
				code = xe.ExitCode()
			}
			s.ch <- p.event(waggle.KindDied, waggle.Enc(waggle.Died{Error: err.Error(), Code: code}))
		}
	}()
	return s, nil
}

type stream struct {
	ch     chan waggle.Event
	cancel func()
}

func (s *stream) Events() <-chan waggle.Event { return s.ch }

func (s *stream) Stop(ctx context.Context) error {
	s.cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Millisecond):
		return nil
	}
}

// parser turns codex exec --json lines into waggle events. Shapes come
// from real transcripts in testdata; anything unrecognized becomes
// KindRaw so the drift gate can catch upstream changes.
type parser struct {
	sess waggle.SessionID
	bee  string
	seq  uint64
}

type codexLine struct {
	Type   string     `json:"type"`
	Thread string     `json:"thread_id"`
	Item   *codexItem `json:"item"`
	Usage  *struct {
		Input     int64 `json:"input_tokens"`
		Cached    int64 `json:"cached_input_tokens"`
		Output    int64 `json:"output_tokens"`
		Reasoning int64 `json:"reasoning_output_tokens"`
	} `json:"usage"`
	Message string `json:"message"`
}

type codexItem struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Text    string `json:"text"`
	Command string `json:"command"`
	Output  string `json:"aggregated_output"`
	Exit    *int   `json:"exit_code"`
	Status  string `json:"status"`
	Changes []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	} `json:"changes"`
}

func (p *parser) event(k waggle.Kind, data json.RawMessage) waggle.Event {
	p.seq++
	return waggle.Event{Seq: p.seq, Sess: p.sess, Bee: p.bee, Kind: k, At: time.Now(), Data: data}
}

func (p *parser) line(b []byte) []waggle.Event {
	var l codexLine
	if err := json.Unmarshal(b, &l); err != nil {
		if len(b) == 0 {
			return nil
		}
		return []waggle.Event{p.event(waggle.KindRaw, waggle.Enc(waggle.Raw{Line: string(b)}))}
	}
	switch l.Type {
	case "thread.started":
		return []waggle.Event{p.event(waggle.KindSpawned, waggle.Enc(waggle.Spawned{Resume: l.Thread, Brain: name}))}
	case "turn.started":
		return nil // spawn already announced; turn boundary carries no data
	case "turn.completed":
		evs := []waggle.Event{}
		if l.Usage != nil {
			evs = append(evs, p.event(waggle.KindCost, waggle.Enc(waggle.Cost{
				InputTokens:       l.Usage.Input,
				CachedInputTokens: l.Usage.Cached,
				OutputTokens:      l.Usage.Output,
				ReasoningTokens:   l.Usage.Reasoning,
			})))
		}
		return append(evs, p.event(waggle.KindResult, nil))
	case "turn.failed", "error":
		return []waggle.Event{p.event(waggle.KindDied, waggle.Enc(waggle.Died{Error: l.Message}))}
	case "item.started", "item.updated", "item.completed":
		if l.Item == nil {
			break
		}
		return p.item(l)
	}
	return []waggle.Event{p.event(waggle.KindRaw, waggle.Enc(waggle.Raw{Line: string(b)}))}
}

func (p *parser) item(l codexLine) []waggle.Event {
	it := l.Item
	done := l.Type == "item.completed"
	switch it.Type {
	case "agent_message":
		if !done {
			return nil // codex sends the full text at completion
		}
		return []waggle.Event{p.event(waggle.KindMessage, waggle.Enc(waggle.Message{Text: it.Text}))}
	case "reasoning":
		if !done {
			return nil
		}
		return []waggle.Event{p.event(waggle.KindFinding, waggle.Enc(waggle.Message{Text: it.Text}))}
	case "command_execution":
		t := waggle.Tool{Command: it.Command, Status: it.Status}
		if done {
			t.Output = it.Output
			t.ExitCode = it.Exit
		}
		return []waggle.Event{p.event(waggle.KindTool, waggle.Enc(t))}
	case "file_change":
		e := waggle.Edit{Status: it.Status}
		for _, c := range it.Changes {
			e.Changes = append(e.Changes, waggle.FileChange{Path: c.Path, Op: c.Kind})
		}
		return []waggle.Event{p.event(waggle.KindEdit, waggle.Enc(e))}
	case "mcp_tool_call", "web_search":
		t := waggle.Tool{Command: it.Type, Status: it.Status}
		return []waggle.Event{p.event(waggle.KindTool, waggle.Enc(t))}
	case "todo_list":
		return nil // plan chatter; deliberately not rendered at S0
	}
	raw, _ := json.Marshal(l)
	return []waggle.Event{p.event(waggle.KindRaw, waggle.Enc(waggle.Raw{Line: string(raw)}))}
}
