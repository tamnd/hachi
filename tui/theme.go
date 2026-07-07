package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// theme groups every style the TUI uses. It is built for the terminal's
// actual background (queried at startup) so light and dark both read well.
type theme struct {
	dark bool

	Header    lipgloss.Style
	HeaderSub lipgloss.Style
	BrainChip lipgloss.Style

	HumanBar  lipgloss.Style
	Human     lipgloss.Style
	Finding   lipgloss.Style
	ToolCmd   lipgloss.Style
	ToolOut   lipgloss.Style
	ToolOK    lipgloss.Style
	ToolBad   lipgloss.Style
	ToolDur   lipgloss.Style
	EditPath  lipgloss.Style
	EditAdd   lipgloss.Style
	EditDel   lipgloss.Style
	DiedBox   lipgloss.Style
	Composer  lipgloss.Style
	StatusBar lipgloss.Style
	StatusKey lipgloss.Style
	Brain     lipgloss.Style
	Faint     lipgloss.Style
	Title     lipgloss.Style
	ListBox   lipgloss.Style
	ListSel   lipgloss.Style
	Logo      lipgloss.Style
	BoardCard lipgloss.Style
	BoardNeed lipgloss.Style
	BoardSel  lipgloss.Style
}

func newTheme(dark bool) theme {
	ld := lipgloss.LightDark(dark)
	pick := func(light, darkc string) color.Color {
		return ld(lipgloss.Color(light), lipgloss.Color(darkc))
	}

	honey := pick("#B58900", "#E6B450")
	subtle := pick("#767676", "#8A8A8A")
	good := pick("#2AA198", "#5AF78E")
	bad := pick("#DC322F", "#FF5C57")
	box := pick("#D0D0D0", "#3A3A3A")
	chipFg := pick("#FFFFFF", "#1A1A1A")

	return theme{
		dark:      dark,
		Header:    lipgloss.NewStyle().Foreground(honey).Bold(true),
		HeaderSub: lipgloss.NewStyle().Foreground(subtle),
		BrainChip: lipgloss.NewStyle().Background(honey).Foreground(chipFg).Padding(0, 1).Bold(true),
		HumanBar:  lipgloss.NewStyle().Foreground(honey).Bold(true),
		Human:     lipgloss.NewStyle().Bold(true),
		Finding:   lipgloss.NewStyle().Foreground(subtle).Italic(true),
		ToolCmd:   lipgloss.NewStyle().Bold(true),
		ToolOut:   lipgloss.NewStyle().Foreground(subtle),
		ToolOK:    lipgloss.NewStyle().Foreground(good),
		ToolBad:   lipgloss.NewStyle().Foreground(bad).Bold(true),
		ToolDur:   lipgloss.NewStyle().Foreground(subtle),
		EditPath:  lipgloss.NewStyle().Foreground(honey),
		EditAdd:   lipgloss.NewStyle().Foreground(good),
		EditDel:   lipgloss.NewStyle().Foreground(bad),
		DiedBox:   lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(bad).Padding(0, 1).Foreground(bad),
		Composer:  lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(honey).Padding(0, 1),
		StatusBar: lipgloss.NewStyle().Foreground(subtle),
		StatusKey: lipgloss.NewStyle().Foreground(honey),
		Brain:     lipgloss.NewStyle().Foreground(honey).Bold(true),
		Faint:     lipgloss.NewStyle().Foreground(subtle),
		Title:     lipgloss.NewStyle().Bold(true).Foreground(honey),
		ListBox:   lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(box).Padding(1, 2),
		ListSel:   lipgloss.NewStyle().Foreground(honey).Bold(true),
		Logo:      lipgloss.NewStyle().Foreground(honey).Bold(true),
		BoardCard: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(box).Padding(0, 1),
		BoardNeed: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(bad).Padding(0, 1),
		BoardSel:  lipgloss.NewStyle().Border(lipgloss.ThickBorder()).BorderForeground(honey).Padding(0, 1),
	}
}
