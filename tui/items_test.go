package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/tamnd/hachi/waggle"
)

// gallery holds deliberately ugly inputs: the renderer must survive all
// of them at any width without panicking or leaking control bytes.
var gallery = []waggle.Event{
	{Kind: waggle.KindMessage, Bee: "human", Data: waggle.Enc(waggle.Message{Text: "please fix the tests"})},
	{Kind: waggle.KindMessage, Bee: "codex", Data: waggle.Enc(waggle.Message{Text: "Done. Here is why:\n\n```go\nfunc main() {\n\tfmt.Println(\"a very long line that should wrap somewhere reasonable instead of running off the edge of the terminal\")\n}\n```\n\n- one\n- two"})},
	{Kind: waggle.KindMessage, Bee: "codex", Data: waggle.Enc(waggle.Message{Text: "日本語のテキストは幅が二倍になるので折り返し計算が壊れやすい。蜂蜜のように甘い出力を。"})},
	{Kind: waggle.KindMessage, Bee: "human", Data: waggle.Enc(waggle.Message{Text: strings.Repeat("x", 500)})},
	{Kind: waggle.KindFinding, Bee: "codex", Data: waggle.Enc(waggle.Message{Text: "I should check the Makefile first"})},
	{Kind: waggle.KindTool, Bee: "codex", Data: waggle.Enc(waggle.Tool{Ref: "item_1", Command: "go test ./...", Status: "in_progress"})},
	{Kind: waggle.KindTool, Bee: "codex", Data: waggle.Enc(waggle.Tool{Ref: "item_1", Command: "go test ./...", Status: "completed", ExitCode: intp(0), Output: "ok  \tgithub.com/tamnd/hachi\t0.3s\n" + strings.Repeat("line of output\n", 20)})},
	{Kind: waggle.KindTool, Bee: "codex", Data: waggle.Enc(waggle.Tool{Ref: "item_2", Command: "cat /bin/ls", Status: "completed", ExitCode: intp(1), Output: "\x1b[31mred\x1b[0m and \x1b]0;title\x07osc and \x07bell and \r\x00junk"})},
	{Kind: waggle.KindTool, Bee: "codex", Data: waggle.Enc(waggle.Tool{Command: strings.Repeat("very-long-command ", 40), Status: "completed"})},
	{Kind: waggle.KindEdit, Bee: "codex", Data: waggle.Enc(waggle.Edit{Ref: "item_3", Status: "in_progress"})},
	{Kind: waggle.KindEdit, Bee: "codex", Data: waggle.Enc(waggle.Edit{Ref: "item_3", Status: "completed", Changes: []waggle.FileChange{{Path: "tui/app.go", Op: "update"}, {Path: "tui/new.go", Op: "add"}, {Path: "tui/old.go", Op: "delete"}}})},
	{Kind: waggle.KindPlan, Bee: "codex", Data: waggle.Enc(waggle.Plan{Ref: "item_4", Items: []waggle.PlanItem{
		{Text: "Scaffold the repo and CI", Done: true},
		{Text: "Implement the game loop with a deliberately overlong step description that has to be truncated somewhere sensible before it wrecks the layout of the card", Done: false},
		{Text: "写测试并修复所有问题", Done: false},
	}})},
	{Kind: waggle.KindDied, Bee: "codex", Data: waggle.Enc(waggle.Died{Error: "stream error: exceeded retry limit"})},
	{Kind: waggle.KindRaw, Bee: "codex", Data: waggle.Enc(waggle.Raw{Line: `{"type":"mystery"}`})},
}

func intp(n int) *int { return &n }

func TestRenderGallery(t *testing.T) {
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	for _, dark := range []bool{true, false} {
		for _, width := range []int{80, 120, 200} {
			rc := renderCtx{
				th: newTheme(dark), md: newMDCache(dark), width: width,
				frame: "⠋", expanded: false, now: base.Add(3 * time.Second),
			}
			for i, ev := range gallery {
				ev.At = base.Add(time.Duration(i) * time.Second)
				it := &item{ev: ev, start: base}
				out := it.render(rc)
				for _, line := range strings.Split(out, "\n") {
					if w := lipgloss.Width(line); w > width {
						t.Errorf("dark=%v width=%d event %d (%s): line width %d overflows:\n%q",
							dark, width, i, ev.Kind, w, line)
					}
					if strings.ContainsAny(line, "\x00\x07\x08\r") {
						t.Errorf("width=%d event %d (%s): control bytes leaked: %q", width, i, ev.Kind, line)
					}
				}
			}
		}
	}
}

func TestRenderExpandedShowsMore(t *testing.T) {
	ev := gallery[6] // completed go test with 21 lines of output
	rc := renderCtx{th: newTheme(true), md: newMDCache(true), width: 100, now: time.Now()}
	clipped := (&item{ev: ev}).render(rc)
	rc.expanded = true
	full := (&item{ev: ev}).render(rc)
	if strings.Count(full, "\n") <= strings.Count(clipped, "\n") {
		t.Fatalf("expanded output should have more lines: clipped %d, full %d",
			strings.Count(clipped, "\n"), strings.Count(full, "\n"))
	}
}

func TestRunningCardsNeverFreeze(t *testing.T) {
	rc := renderCtx{th: newTheme(true), md: newMDCache(true), width: 80, frame: "⠋", now: time.Now()}
	it := &item{ev: gallery[5], start: time.Now().Add(-2 * time.Second)} // in_progress tool
	if out := it.render(rc); !strings.Contains(out, "⠋") || !strings.Contains(out, "2s") {
		t.Fatalf("running tool card must show the spinner and elapsed time: %q", out)
	}
	if it.frozen {
		t.Fatal("in_progress tool card must not freeze; it has to keep animating")
	}
	itDone := &item{ev: gallery[6], start: time.Now()}
	itDone.render(rc)
	if !itDone.frozen {
		t.Fatal("completed tool card should freeze for the render cache")
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"plain text":                   "plain text",
		"\x1b[31mred\x1b[0m":           "red",
		"\x1b]0;title\x07after":        "after",
		"keep\nnewline\tand tab":       "keep\nnewline\tand tab",
		"drop\rcarriage\x00null\x07":   "dropcarriagenull",
		"\x1b[38;2;255;0;0mtruecolor✓": "truecolor✓",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestApplyRefCorrelation checks that an item.completed event replaces the
// in_progress card in place instead of appending a duplicate.
func TestApplyRefCorrelation(t *testing.T) {
	m := newModel(nil, Options{})
	m.w, m.h = 100, 40
	m.layout()

	m.apply(waggle.Event{Seq: 1, Bee: "codex", Kind: waggle.KindTool, At: time.Now(),
		Data: waggle.Enc(waggle.Tool{Ref: "item_0", Command: "make", Status: "in_progress"})})
	if len(m.items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(m.items))
	}
	m.apply(waggle.Event{Seq: 2, Bee: "codex", Kind: waggle.KindTool, At: time.Now(),
		Data: waggle.Enc(waggle.Tool{Ref: "item_0", Command: "make", Status: "completed", ExitCode: intp(0), Output: "done"})})
	if len(m.items) != 1 {
		t.Fatalf("completed event with same ref must update in place; got %d items", len(m.items))
	}
	tool, ok := decode[waggle.Tool](m.items[0].ev.Data)
	if !ok || tool.Status != "completed" {
		t.Fatalf("card not updated: %+v", tool)
	}

	// A different ref appends.
	m.apply(waggle.Event{Seq: 3, Bee: "codex", Kind: waggle.KindTool, At: time.Now(),
		Data: waggle.Enc(waggle.Tool{Ref: "item_1", Command: "ls", Status: "in_progress"})})
	if len(m.items) != 2 {
		t.Fatalf("new ref should append; got %d items", len(m.items))
	}
}

// TestApplyCostAccumulates checks token accounting across turns, including
// the cached-input breakdown shown in the status bar.
func TestApplyCostAccumulates(t *testing.T) {
	m := newModel(nil, Options{})
	for i := 0; i < 2; i++ {
		m.apply(waggle.Event{Seq: uint64(10 + i), Kind: waggle.KindCost, At: time.Now(),
			Data: waggle.Enc(waggle.Cost{InputTokens: 100, CachedInputTokens: 40, OutputTokens: 7})})
	}
	if m.tokens.InputTokens != 200 || m.tokens.CachedInputTokens != 80 || m.tokens.OutputTokens != 14 {
		t.Fatalf("cost accumulation wrong: %+v", m.tokens)
	}
}

// TestApplyPulse checks that pulses replace each other and the settled
// cost at turn end never double counts what pulses already showed, while
// the slow-moving vitals (model, context, limits) survive the settle.
func TestApplyPulse(t *testing.T) {
	m := newModel(nil, Options{})
	m.apply(waggle.Event{Seq: 1, Kind: waggle.KindPulse, At: time.Now(),
		Data: waggle.Enc(waggle.Pulse{Usage: waggle.Cost{InputTokens: 100, OutputTokens: 10}})})
	m.apply(waggle.Event{Seq: 2, Kind: waggle.KindPulse, At: time.Now(),
		Data: waggle.Enc(waggle.Pulse{
			Usage:   waggle.Cost{InputTokens: 250, OutputTokens: 30},
			Context: 280, Window: 258400, Model: "gpt-5-codex", Effort: "low",
			Limits: []waggle.Limit{{Name: "5h", UsedPct: 1}},
		})})
	if m.pulse.Usage.InputTokens != 250 || m.pulse.Usage.OutputTokens != 30 {
		t.Fatalf("pulse must replace, not add: %+v", m.pulse.Usage)
	}
	if m.tokens.InputTokens != 0 {
		t.Fatalf("pulses must not touch settled tokens: %+v", m.tokens)
	}
	m.apply(waggle.Event{Seq: 3, Kind: waggle.KindCost, At: time.Now(),
		Data: waggle.Enc(waggle.Cost{InputTokens: 260, OutputTokens: 32})})
	if m.tokens.InputTokens != 260 || m.tokens.OutputTokens != 32 {
		t.Fatalf("settled cost wrong: %+v", m.tokens)
	}
	if m.pulse.Usage.InputTokens != 0 || m.pulse.Usage.OutputTokens != 0 {
		t.Fatalf("settle must clear the pulse usage: %+v", m.pulse.Usage)
	}
	if m.pulse.Model != "gpt-5-codex" || m.pulse.Window != 258400 || len(m.pulse.Limits) != 1 {
		t.Fatalf("settle must keep the slow vitals: %+v", m.pulse)
	}
}

// TestApplyLegacyLiveCost keeps older journals rendering: live cost
// snapshots predate pulses and must still replace, never accumulate.
func TestApplyLegacyLiveCost(t *testing.T) {
	m := newModel(nil, Options{})
	m.apply(waggle.Event{Seq: 1, Kind: waggle.KindCost, At: time.Now(),
		Data: waggle.Enc(waggle.Cost{InputTokens: 100, OutputTokens: 10, Live: true})})
	m.apply(waggle.Event{Seq: 2, Kind: waggle.KindCost, At: time.Now(),
		Data: waggle.Enc(waggle.Cost{InputTokens: 250, OutputTokens: 30, Live: true})})
	if m.pulse.Usage.InputTokens != 250 || m.tokens.InputTokens != 0 {
		t.Fatalf("legacy live cost handled wrong: pulse=%+v settled=%+v", m.pulse.Usage, m.tokens)
	}
}

// TestVitalsInStatus checks the status bar carries the pulse vitals.
func TestVitalsInStatus(t *testing.T) {
	m := newModel(nil, Options{})
	m.w, m.h = 160, 40
	m.pulse = waggle.Pulse{
		Context: 51680, Window: 258400, Model: "gpt-5-codex", Effort: "low",
		Limits: []waggle.Limit{{Name: "5h", UsedPct: 1}, {Name: "week", UsedPct: 85}},
	}
	s := m.viewStatus()
	for _, want := range []string{"gpt-5-codex low", "20% context", "5h 1%", "week 85%"} {
		if !strings.Contains(s, want) {
			t.Fatalf("status bar missing %q:\n%s", want, s)
		}
	}
}
