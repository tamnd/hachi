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
	"strings"
	"sync"
	"sync/atomic"
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
	// Sessions overrides the rollout log directory scanned for live token
	// counts; empty means $CODEX_HOME (or ~/.codex) plus /sessions.
	Sessions string
}

// Detect reports whether the codex binary is on PATH.
func (d *Driver) Detect() error {
	_, err := exec.LookPath(d.Bin)
	return err
}

// runArgs builds the codex exec invocation for one turn. Two properties
// matter for cost: every follow-up turn resumes the stored thread, and the
// flags never vary between turns. Both keep the conversation prefix
// byte-identical so the provider's prompt cache keeps serving it; a fresh
// thread or a changed profile would re-bill the whole history as uncached
// input.
func (d *Driver) runArgs(resume, msg string) []string {
	// The resume subcommand only takes --json and --skip-git-repo-check;
	// sandbox flags belong to exec and must come before it.
	sandbox := []string{"--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=true"}
	common := []string{"--json", "--skip-git-repo-check"}
	args := append([]string{"exec"}, sandbox...)
	if resume != "" {
		args = append(args, "resume", resume)
	}
	args = append(args, common...)
	args = append(args, d.Args...)
	return append(args, msg)
}

// Run starts one turn: a fresh thread when sess.Resume is empty, otherwise
// codex exec resume with the stored thread id. The sandbox is
// workspace-write with network on: exec mode cannot ask for approval, and
// a brain that cannot fetch a dependency or push a branch is not doing
// real work. Anything outside the workspace stays off limits.
func (d *Driver) Run(ctx context.Context, sess adapter.Session, msg string) (adapter.Stream, error) {
	cmd := exec.CommandContext(ctx, d.Bin, d.runArgs(sess.Resume, msg)...)
	cmd.Dir = sess.Dir
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Keep the tail of stderr: on a nonzero exit it is the only clue
	// (bad flag, auth failure), and "exit status 2" alone helps nobody.
	errTail := &tailBuffer{max: 4096}
	cmd.Stderr = errTail
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	s := &stream{ch: make(chan waggle.Event, 64)}
	s.cancel = func() {
		s.stopped.Store(true)
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	go func() {
		defer close(s.ch)
		// Pulses run on their own goroutine, so wind them down before the
		// channel closes: cancel, then wait.
		pctx, pcancel := context.WithCancel(ctx)
		var pulses sync.WaitGroup
		defer func() {
			pcancel()
			pulses.Wait()
		}()
		p := parser{sess: sess.ID, bee: name, merge: newPatchMerge()}
		pulsing := false
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		done := false
		for !done && sc.Scan() {
			for _, ev := range p.line(sc.Bytes()) {
				if ev.Kind == waggle.KindSpawned && !pulsing {
					pulsing = true
					var sp waggle.Spawned
					if json.Unmarshal(ev.Data, &sp) == nil && sp.Resume != "" {
						fromStart := sess.Resume == ""
						pulses.Add(1)
						go func() {
							defer pulses.Done()
							tailRollout(pctx, d.sessionsRoot(), sp.Resume, fromStart, func(pu waggle.Pulse) {
								select {
								case s.ch <- p.event(waggle.KindPulse, waggle.Enc(pu)):
								case <-pctx.Done():
								}
							}, func(diffs map[string]string) {
								for _, e := range p.merge.apply(diffs) {
									select {
									case s.ch <- p.event(waggle.KindEdit, waggle.Enc(e)):
									case <-pctx.Done():
										return
									}
								}
							})
						}()
					}
				}
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
		// A user-initiated stop is a clean end of the turn, not a death.
		if err != nil && ctx.Err() == nil && !s.stopped.Load() {
			var xe *exec.ExitError
			code := -1
			if errors.As(err, &xe) {
				code = xe.ExitCode()
			}
			msg := err.Error()
			if tail := strings.TrimSpace(errTail.String()); tail != "" {
				msg += ": " + tail
			}
			s.ch <- p.event(waggle.KindDied, waggle.Enc(waggle.Died{Error: msg, Code: code}))
		}
	}()
	return s, nil
}

type stream struct {
	ch      chan waggle.Event
	cancel  func()
	stopped atomic.Bool
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
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
// seq is atomic because the pulse goroutine stamps events concurrently
// with the stdout parse loop.
type parser struct {
	sess  waggle.SessionID
	bee   string
	seq   atomic.Uint64
	merge *patchMerge
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
	Items []struct {
		Text      string `json:"text"`
		Completed bool   `json:"completed"`
	} `json:"items"`
}

func (p *parser) event(k waggle.Kind, data json.RawMessage) waggle.Event {
	return waggle.Event{Seq: p.seq.Add(1), Sess: p.sess, Bee: p.bee, Kind: k, At: time.Now(), Data: data}
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
		t := waggle.Tool{Ref: it.ID, Command: it.Command, Status: it.Status}
		if done {
			t.Output = it.Output
			t.ExitCode = it.Exit
		}
		return []waggle.Event{p.event(waggle.KindTool, waggle.Enc(t))}
	case "file_change":
		e := waggle.Edit{Ref: it.ID, Status: it.Status}
		for _, c := range it.Changes {
			e.Changes = append(e.Changes, waggle.FileChange{Path: c.Path, Op: c.Kind})
		}
		// The rollout tail may already hold this card's diffs, or will
		// deliver them moments after the card appears.
		p.merge.fold(&e)
		return []waggle.Event{p.event(waggle.KindEdit, waggle.Enc(e))}
	case "mcp_tool_call", "web_search":
		t := waggle.Tool{Ref: it.ID, Command: it.Type, Status: it.Status}
		return []waggle.Event{p.event(waggle.KindTool, waggle.Enc(t))}
	case "todo_list":
		pl := waggle.Plan{Ref: it.ID}
		for _, item := range it.Items {
			pl.Items = append(pl.Items, waggle.PlanItem{Text: item.Text, Done: item.Completed})
		}
		return []waggle.Event{p.event(waggle.KindPlan, waggle.Enc(pl))}
	}
	raw, _ := json.Marshal(l)
	return []waggle.Event{p.event(waggle.KindRaw, waggle.Enc(waggle.Raw{Line: string(raw)}))}
}
