package tui

import "github.com/charmbracelet/lipgloss"

// theme groups every style the TUI uses. Colors are adaptive so light and
// dark terminals both read well.
type theme struct {
	Human     lipgloss.Style
	HumanTag  lipgloss.Style
	Finding   lipgloss.Style
	ToolBox   lipgloss.Style
	ToolCmd   lipgloss.Style
	ToolOut   lipgloss.Style
	ToolOK    lipgloss.Style
	ToolBad   lipgloss.Style
	EditBox   lipgloss.Style
	EditPath  lipgloss.Style
	DiedBox   lipgloss.Style
	StatusBar lipgloss.Style
	StatusKey lipgloss.Style
	Brain     lipgloss.Style
	Faint     lipgloss.Style
	Title     lipgloss.Style
	ListSel   lipgloss.Style
	ListRow   lipgloss.Style
	Logo      lipgloss.Style
}

func newTheme() theme {
	accent := lipgloss.AdaptiveColor{Light: "#B58900", Dark: "#E6B450"} // honey
	subtle := lipgloss.AdaptiveColor{Light: "#6C6C6C", Dark: "#8A8A8A"}
	good := lipgloss.AdaptiveColor{Light: "#2AA198", Dark: "#5AF78E"}
	bad := lipgloss.AdaptiveColor{Light: "#DC322F", Dark: "#FF5C57"}
	box := lipgloss.AdaptiveColor{Light: "#DADADA", Dark: "#3A3A3A"}

	return theme{
		Human:     lipgloss.NewStyle().Bold(true),
		HumanTag:  lipgloss.NewStyle().Foreground(accent).Bold(true),
		Finding:   lipgloss.NewStyle().Foreground(subtle).Italic(true),
		ToolBox:   lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(box).Padding(0, 1),
		ToolCmd:   lipgloss.NewStyle().Bold(true),
		ToolOut:   lipgloss.NewStyle().Foreground(subtle),
		ToolOK:    lipgloss.NewStyle().Foreground(good),
		ToolBad:   lipgloss.NewStyle().Foreground(bad).Bold(true),
		EditBox:   lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 1),
		EditPath:  lipgloss.NewStyle().Foreground(accent),
		DiedBox:   lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(bad).Padding(0, 1).Foreground(bad),
		StatusBar: lipgloss.NewStyle().Foreground(subtle),
		StatusKey: lipgloss.NewStyle().Foreground(accent),
		Brain:     lipgloss.NewStyle().Foreground(accent).Bold(true),
		Faint:     lipgloss.NewStyle().Foreground(subtle),
		Title:     lipgloss.NewStyle().Bold(true).Foreground(accent),
		ListSel:   lipgloss.NewStyle().Foreground(accent).Bold(true),
		ListRow:   lipgloss.NewStyle(),
		Logo:      lipgloss.NewStyle().Foreground(accent).Bold(true),
	}
}
