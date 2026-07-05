// Package waggle defines the event stream every brain and bee in hachi emits.
// The Event is the one contract in the system: adapters translate into it,
// the journal persists it, and every client renders it.
package waggle

import (
	"encoding/json"
	"time"
)

// Kind classifies an event. The set is closed on purpose: an adapter that
// sees upstream output it cannot classify must emit KindRaw, which trips
// the drift gate in CI before it surprises a user.
type Kind string

const (
	// KindSpawned marks a run starting. Data carries the adapter's resume
	// handle when one exists (codex thread_id, claude session_id).
	KindSpawned Kind = "spawned"
	// KindMessage is prose from the brain meant for the human.
	KindMessage Kind = "message"
	// KindTool is a tool or command execution, with a lifecycle status.
	KindTool Kind = "tool"
	// KindEdit is a file change, with a lifecycle status.
	KindEdit Kind = "edit"
	// KindFinding is intermediate reasoning or progress the brain surfaced.
	KindFinding Kind = "finding"
	// KindPlan is the brain's working checklist, replacing earlier
	// versions of itself as steps complete.
	KindPlan Kind = "plan"
	// KindNeedInput means the run is blocked on the human.
	KindNeedInput Kind = "need_input"
	// KindPulse is a periodic snapshot of a running brain's vitals, read
	// from whatever the brain writes on disk while it works. Each pulse
	// carries the whole picture so far and replaces the previous one.
	KindPulse Kind = "pulse"
	// KindCost carries token usage for a completed turn.
	KindCost Kind = "cost"
	// KindResult marks a turn finishing cleanly.
	KindResult Kind = "result"
	// KindDied marks a run ending abnormally.
	KindDied Kind = "died"
	// KindRaw wraps upstream output no mapping exists for yet.
	KindRaw Kind = "raw"
)

// SessionID identifies a conversation in the hive.
type SessionID string

// Event is one observation from a running brain or bee.
type Event struct {
	Seq  uint64          `json:"seq"`
	Sess SessionID       `json:"sess"`
	Bee  string          `json:"bee"`
	Kind Kind            `json:"kind"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Payload shapes for Data, one struct per kind that carries structure.
// Adapters fill these; clients decode the ones they render.

// Spawned is the payload for KindSpawned.
type Spawned struct {
	Resume string `json:"resume,omitempty"`
	Brain  string `json:"brain,omitempty"`
}

// Message is the payload for KindMessage and KindFinding.
type Message struct {
	Text string `json:"text"`
}

// Tool is the payload for KindTool. Ref correlates the lifecycle of one
// tool call: a client that sees two Tool events with the same Ref updates
// the earlier card in place instead of appending a new one.
type Tool struct {
	Ref      string `json:"ref,omitempty"`
	Command  string `json:"command"`
	Output   string `json:"output,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Status   string `json:"status"` // in_progress | completed | failed
}

// FileChange is one entry in an Edit payload. Diff, when the brain
// reports one, holds unified-diff style lines: hunk headers, context, and
// +/- changes. New files show their content as all additions.
type FileChange struct {
	Path string `json:"path"`
	Op   string `json:"op"` // add | update | delete
	Diff string `json:"diff,omitempty"`
}

// Edit is the payload for KindEdit. Ref works as in Tool.
type Edit struct {
	Ref     string       `json:"ref,omitempty"`
	Changes []FileChange `json:"changes"`
	Status  string       `json:"status"`
}

// PlanItem is one step in a Plan.
type PlanItem struct {
	Text string `json:"text"`
	Done bool   `json:"done"`
}

// Plan is the payload for KindPlan. Ref works as in Tool: each update
// carries the full list and replaces the earlier card.
type Plan struct {
	Ref   string     `json:"ref,omitempty"`
	Items []PlanItem `json:"items"`
}

// Pulse is the payload for KindPulse. Usage counts the turn so far and
// mirrors what the settled Cost will say; Context and Window describe how
// full the model's context is right now; Limits are the provider's
// rate-limit meters when the brain reports them. Any field the brain does
// not know stays zero.
type Pulse struct {
	Usage   Cost    `json:"usage"`
	Context int64   `json:"context,omitempty"` // tokens sitting in the model's window
	Window  int64   `json:"window,omitempty"`  // the window's total size
	Model   string  `json:"model,omitempty"`
	Effort  string  `json:"effort,omitempty"`
	Limits  []Limit `json:"limits,omitempty"`
}

// Limit is one provider rate-limit meter, like the 5h and weekly windows
// on a subscription plan.
type Limit struct {
	Name     string    `json:"name"`
	UsedPct  float64   `json:"used_pct"`
	ResetsAt time.Time `json:"resets_at,omitzero"`
}

// Cost is the payload for KindCost: token usage settled at the end of a
// turn. Costs accumulate across turns. Live marks an in-flight snapshot
// rather than a settled total; adapters emit KindPulse for that now, and
// the field stays so older journals replay without double counting.
type Cost struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	ReasoningTokens   int64 `json:"reasoning_tokens"`
	Live              bool  `json:"live,omitempty"`
}

// Died is the payload for KindDied.
type Died struct {
	Error string `json:"error,omitempty"`
	Code  int    `json:"code,omitempty"`
}

// Raw is the payload for KindRaw: the upstream line kept verbatim.
type Raw struct {
	Line string `json:"line"`
}

// Enc marshals a payload into Data, panicking only on programmer error
// (payload structs are always marshalable).
func Enc(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("waggle: unmarshalable payload: " + err.Error())
	}
	return b
}
