package tui

import "github.com/charmbracelet/lipgloss"

// A dark, high-contrast palette close to the OpenCode TUI.
var (
	colAccent  = lipgloss.Color("#8B5CF6") // violet
	colAccent2 = lipgloss.Color("#22D3EE") // cyan
	colText    = lipgloss.Color("#E5E7EB")
	colMuted   = lipgloss.Color("#6B7280")
	colGreen   = lipgloss.Color("#22C55E")
	colRed     = lipgloss.Color("#EF4444")
	colYellow  = lipgloss.Color("#EAB308")
	colPanel   = lipgloss.Color("#0F172A")
	colBorder  = lipgloss.Color("#1F2937")
	colSelBg   = lipgloss.Color("#312E81")
)

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colAccent2)

	headerStyle = lipgloss.NewStyle().
			Foreground(colText).
			Padding(0, 1)

	addrStyle = lipgloss.NewStyle().
			Foreground(colMuted)

	mutedStyle = lipgloss.NewStyle().
			Foreground(colMuted)

	dimStyle = lipgloss.NewStyle().
			Foreground(colMuted).
			Italic(true)

	contentStyle = lipgloss.NewStyle().
			Foreground(colText)

	reasoningStyle = lipgloss.NewStyle().
			Foreground(colMuted).
			Italic(true)

	accentStyle = lipgloss.NewStyle().
			Foreground(colAccent2)

	successStyle = lipgloss.NewStyle().
			Foreground(colGreen)

	dangerStyle = lipgloss.NewStyle().
			Foreground(colRed)

	warnStyle = lipgloss.NewStyle().
			Foreground(colYellow)

	boldStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colText)

	sidebarStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colBorder).
			Padding(0, 1)

	mainStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colBorder).
			Padding(0, 1)

	inputStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colAccent).
			Padding(0, 1)

	selectedItemStyle = lipgloss.NewStyle().
				Background(colSelBg).
				Foreground(colText).
				Bold(true)

	itemStyle = lipgloss.NewStyle().
			Foreground(colText)

	tabActiveStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colAccent2).
			Underline(true)

	tabInactiveStyle = lipgloss.NewStyle().
				Foreground(colMuted)

	statusBarStyle = lipgloss.NewStyle().
			Foreground(colMuted).
			Padding(0, 1)

	badgeStyle = lipgloss.NewStyle().
			Padding(0, 1).
			Bold(true)
)

// statusBadge renders a coloured status pill.
func statusBadge(status string) string {
	switch status {
	case "completed":
		return badgeStyle.Foreground(colGreen).Render("● completed")
	case "failed":
		return badgeStyle.Foreground(colRed).Render("● failed")
	case "running":
		return badgeStyle.Foreground(colYellow).Render("● running")
	case "queued":
		return badgeStyle.Foreground(colMuted).Render("● queued")
	case "interrupted":
		return badgeStyle.Foreground(colRed).Render("● interrupted")
	default:
		if status == "" {
			status = "unknown"
		}
		return badgeStyle.Foreground(colMuted).Render("● " + status)
	}
}
