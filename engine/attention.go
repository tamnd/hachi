package engine

// Attention is why a session needs the human, one reason per session.
// The engine raises it, because codex runs headless and never blocks on
// stdin: a question only becomes an ask because hachi noticed the final
// message ends with one, and a finished diff only calls for eyes because
// hachi knows nobody accepted it. Reasons live in memory like the rest
// of the derived state; the journal keeps the need_input and marker
// events so a future replay can reach the same answer.

import (
	"context"
	"strings"
	"time"

	"github.com/tamnd/hachi/hive"
	"github.com/tamnd/hachi/waggle"
)

// attention is one raised reason. At most one per session: a question
// outranks a fresh diff, and a death outranks everything, so whichever
// is raised is already the one worth saying.
type attention struct {
	reason string // "question" | "diff" | "died"
	detail string // one plain sentence: the question, the error, what finished
	raised time.Time
}

// settle decides what a finished turn leaves raised, and returns the
// reason it newly raised, empty when it raised nothing. Called at pump
// end with the turn's evidence: whether a result event arrived (a clean
// finish; an interrupted stream never sends one), the brain's last
// message, and the death detail when the run died. It leaves mid-turn
// asks alone unless the turn was interrupted, because stopping a turn
// is the human acting on it.
func (e *Engine) settle(id waggle.SessionID, final hive.State, sawResult bool, lastSay, diedDetail string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a := e.attn[id]; a != nil {
		if !sawResult && final != hive.StateDied {
			delete(e.attn, id)
		}
		return ""
	}
	a := &attention{raised: time.Now()}
	switch {
	case final == hive.StateDied:
		a.reason, a.detail = "died", diedDetail
	case !sawResult:
		// Interrupted: the human is present and steering; nothing to raise.
		return ""
	case looksLikeQuestion(lastSay):
		a.reason, a.detail = "question", question(lastSay)
	case e.dirty[id]:
		a.reason, a.detail = "diff", "finished with changes to review"
	default:
		return ""
	}
	e.attn[id] = a
	return a.reason
}

// Seen records that the human looked at what the session raised. A fresh
// diff parks back in review, a death is acknowledged and the session
// reads idle again. A question stays raised: it is cleared by answering,
// not by looking. The marker event keeps the acknowledgment on disk.
func (e *Engine) Seen(ctx context.Context, id waggle.SessionID) error {
	e.ensureSeq(id)
	e.mu.Lock()
	a := e.attn[id]
	died := e.state[id] == hive.StateDied
	if (a == nil && !died) || (a != nil && a.reason == "question") {
		e.mu.Unlock()
		return nil
	}
	delete(e.attn, id)
	if died {
		e.state[id] = hive.StateIdle
	}
	e.mu.Unlock()
	e.append(waggle.Event{Sess: id, Bee: "hachi", Kind: waggle.KindMarker, At: time.Now(),
		Data: waggle.Enc(waggle.Marker{Name: "seen"})})
	return nil
}

// looksLikeQuestion says whether a final message reads as the brain
// waiting on an answer: its last non-empty line ends with a question
// mark, once trailing markdown dressing is peeled off.
func looksLikeQuestion(text string) bool {
	return question(text) != ""
}

// question returns the asking line of a message, or empty when the
// message does not end on one.
func question(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		l = strings.TrimRight(l, " \t*_`)]\"'")
		if l == "" {
			continue
		}
		if strings.HasSuffix(l, "?") || strings.HasSuffix(l, "？") {
			return l
		}
		return ""
	}
	return ""
}
