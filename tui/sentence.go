package tui

// The sentence view: the same review state in plain words, for people a
// unified diff shuts out. Same engine verbs behind different labels:
// Keep these changes is a blanket a, Undo everything is a blanket d.
// The words stage, commit, index, HEAD, branch, repository, diff, hunk,
// and untracked never appear on this screen. Sessions outside a git
// repo land here by default; s flips to the file tree either way, and
// the toggle sticks for the rest of the run.

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/tamnd/hachi/hive"
)

func (m *model) keySentence(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.quitAll()
		return m, tea.Quit
	case "q", "esc":
		m.screen = screenChat
		return m, nil
	case "s", "d":
		// The real changes stay one key away; plenty of non-coders are
		// one curiosity away from being coders.
		m.rvPlain, m.rvChose = false, true
		m.renderReview()
		return m, nil
	case "tab", "left", "right":
		m.rvBtn = 1 - m.rvBtn
		return m, nil
	case "enter":
		if m.rvBtn == 1 {
			m.rvConfirm = "*"
			return m, nil
		}
		return m, m.keep(nil)
	case "u":
		m.rvConfirm = "*"
		return m, nil
	case "r":
		return m.requestChanges()
	case "R":
		return m, m.loadDiff()
	}
	return m, nil
}

func (m *model) viewSentence() string {
	title := m.sess.Title
	if title == "" {
		title = "this session"
	}
	header := " " + m.th.Title.Render("what changed") + m.th.Faint.Render("  ──  "+truncate(title, m.w/2))

	bodyH := m.h - 3 - m.stripRows()
	if bodyH < 1 {
		bodyH = 1
	}
	var lines []string
	put := func(s string) { lines = append(lines, s) }
	put("")
	if len(m.diff) == 0 {
		if m.diffLoading {
			put("  " + m.th.Faint.Render("looking at your files…"))
		} else {
			put("  " + m.th.Human.Render("The agent has not changed any files."))
		}
	} else {
		n := len(m.diff)
		lead := fmt.Sprintf("Edited %d files in your project.", n)
		if n == 1 {
			lead = "Edited 1 file in your project."
		}
		put("  " + m.th.Human.Render(lead))
		put("")

		// Everything below the lead has to fit above the buttons; the
		// details are one key away, so the tail folds into one line.
		room := bodyH - 6
		if room < 1 {
			room = 1
		}
		shown := len(m.diff)
		if shown > room {
			shown = room - 1
			if shown < 1 {
				shown = 1
			}
		}
		for _, f := range m.diff[:shown] {
			line := "  • " + fileSentence(f)
			if f.Staged {
				line += m.th.Faint.Render(" · kept")
			}
			put(line)
		}
		if rest := len(m.diff) - shown; rest > 0 {
			put("  " + m.th.Faint.Render(fmt.Sprintf("… and %d more", rest)))
		}
		for _, f := range m.diff {
			if f.NoUndo {
				put("  " + m.th.ToolBad.Render("⚠ "+f.Path+" "+f.Note+"; Undo will not cover it"))
			}
		}
		put("")
		put("       " + m.sentenceButton("Keep these changes", 0) + "      " + m.sentenceButton("Undo everything", 1))
	}
	body := lipgloss.NewStyle().Height(bodyH).MaxWidth(m.w).Render(strings.Join(lines, "\n"))
	return header + "\n" + body + "\n" + m.viewSentenceFooter()
}

func (m *model) sentenceButton(label string, idx int) string {
	if m.rvBtn == idx {
		return m.th.ListSel.Render("[ " + label + " ]")
	}
	return m.th.Faint.Render("[ " + label + " ]")
}

func (m *model) viewSentenceFooter() string {
	if m.rvConfirm != "" {
		return " " + m.th.ToolBad.Render("Undo everything this session did? y/n")
	}
	if m.rvAskKeep {
		return " " + m.th.ToolBad.Render(m.keepQuestion())
	}
	hints := " " + m.th.StatusKey.Render("enter") + m.th.Faint.Render(" choose · ") +
		m.th.StatusKey.Render("tab") + m.th.Faint.Render(" switch · ") +
		m.th.StatusKey.Render("s") + m.th.Faint.Render(" show the code · ") +
		m.th.StatusKey.Render("u") + m.th.Faint.Render(" undo everything · ") +
		m.th.StatusKey.Render("q") + m.th.Faint.Render(" back")
	if m.rvStatus != "" {
		return " " + m.th.Finding.Render(m.rvStatus) + m.th.Faint.Render("  ·") + hints
	}
	return hints
}

// keepQuestion is the one sentence that runs before the first keep in a
// non-git folder, because keeping is the one action there that narrows
// what Undo covers.
func (m *model) keepQuestion() string {
	if len(m.rvKeep) == 1 {
		return fmt.Sprintf("Once you keep %s, Undo stops covering it. Keep it? y/n", m.rvKeep[0])
	}
	return "Once you keep these changes, Undo stops covering them. Keep them? y/n"
}

// fileSentence renders one change as a person would say it. The numbers
// come from the same patch the file tree shows; this is a rendering,
// not a second diff engine.
func fileSentence(f hive.FileDiff) string {
	switch {
	case f.Outside:
		return fmt.Sprintf("You also changed %s yourself, so hachi will leave it alone", f.Path)
	case f.Status == "A":
		return "Created " + f.Path
	case f.Status == "D":
		return "Deleted " + f.Path
	}
	adds, dels := patchTally(f.Patch)
	switch {
	case adds > 0 && dels == 0:
		return fmt.Sprintf("Added %s to %s", nLines(adds), f.Path)
	case dels > 0 && adds == 0:
		return fmt.Sprintf("Removed %s from %s", nLines(dels), f.Path)
	case adds+dels > 0:
		n := max(adds, dels)
		return fmt.Sprintf("Changed %s in %s", nLines(n), f.Path)
	}
	return "Changed " + f.Path
}

func nLines(n int) string {
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}
