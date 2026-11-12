package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/asemoq/sv-multi-agent/internal/events"
	"github.com/asemoq/sv-multi-agent/internal/store"
)

// Run starts the interactive terminal client against addr.
func Run(addr string) error {
	model := New(addr)
	program := tea.NewProgram(model, tea.WithAltScreen())
	_, err := program.Run()
	return err
}

type (
	tickMsg  time.Time
	tasksMsg struct {
		tasks []store.Task
		err   error
	}
	healthMsg   struct{ ok bool }
	sseEventMsg struct {
		taskID string
		event  events.Event
	}
	sseClosedMsg struct{ taskID string }
	submitMsg    struct{ taskID string }
	reportMsg    struct {
		taskID string
		text   string
		err    error
	}
	errMsg struct{ err error }
)

// Model is the Bubble Tea model for the TUI.
type Model struct {
	client *Client
	addr   string

	width, height int
	ready         bool

	tasks    []store.Task
	cursor   int
	selected string

	events   []events.Event
	report   string
	reportID string
	tab      int // 0 = live, 1 = report

	input         textinput.Model
	pathInput     textinput.Model
	focus         int // 0 = task list, 1 = prompt, 2 = RTL path (analyze)
	mode          int // 0 = generate, 1 = analyze
	maxIterations int

	viewport viewport.Model
	spinner  spinner.Model

	health     bool
	errMsg     string
	submitting bool

	sseCancel context.CancelFunc
	sseCh     chan events.Event
	subID     string
}

// New builds a TUI model that talks to the service at addr.
func New(addr string) *Model {
	client := NewClient(addr)

	input := textinput.New()
	input.Placeholder = "Describe the SystemVerilog design to generate…"
	input.CharLimit = 4000
	input.Prompt = "▷ "
	input.PromptStyle = accentStyle
	input.TextStyle = contentStyle
	input.Cursor.Style = lipgloss.NewStyle().Foreground(colAccent2)
	input.Focus()

	pathInput := textinput.New()
	pathInput.Placeholder = `C:\path\to\rtl  (analyze mode)`
	pathInput.CharLimit = 1000
	pathInput.Prompt = "rtl ▷ "
	pathInput.PromptStyle = warnStyle
	pathInput.TextStyle = contentStyle
	pathInput.Cursor.Style = lipgloss.NewStyle().Foreground(colAccent2)

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(colAccent)

	return &Model{
		client:        client,
		addr:          client.Base(),
		input:         input,
		pathInput:     pathInput,
		viewport:      viewport.New(0, 0),
		spinner:       sp,
		maxIterations: 5,
		focus:         1,
	}
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		m.fetchTasks(),
		m.fetchHealth(),
		tickCmd(),
	)
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		m.layout()
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.fetchTasks(), m.fetchHealth(), tickCmd())

	case tasksMsg:
		if msg.err != nil {
			m.errMsg = msg.err.Error()
			return m, nil
		}
		m.errMsg = ""
		m.tasks = msg.tasks
		if m.cursor >= len(m.tasks) {
			m.cursor = len(m.tasks) - 1
		}
		if m.cursor < 0 {
			m.cursor = 0
		}
		if m.selected == "" && len(m.tasks) > 0 {
			m.cursor = 0
			return m, m.selectTask(m.tasks[0].ID)
		}
		return m, nil

	case healthMsg:
		m.health = msg.ok
		return m, nil

	case sseEventMsg:
		if msg.taskID != m.subID {
			return m, nil // stale event from a previous subscription
		}
		m.events = append(m.events, msg.event)
		m.refreshViewport()
		if msg.event.Kind == events.KindDone {
			return m, m.fetchTasks()
		}
		return m, waitEvent(msg.taskID, m.sseCh)

	case sseClosedMsg:
		return m, nil

	case submitMsg:
		m.submitting = false
		m.input.SetValue("")
		m.selected = msg.taskID
		m.events = nil
		m.report = ""
		m.reportID = ""
		m.tab = 0
		m.refreshViewport()
		return m, tea.Batch(m.subscribe(msg.taskID), m.fetchTasks())

	case reportMsg:
		if msg.taskID == m.selected {
			if msg.err != nil {
				m.report = ""
				m.errMsg = msg.err.Error()
			} else {
				m.report = msg.text
				m.reportID = msg.taskID
			}
			m.refreshViewport()
		}
		return m, nil

	case errMsg:
		m.submitting = false
		m.errMsg = msg.err.Error()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		if m.sseCancel != nil {
			m.sseCancel()
		}
		return m, tea.Quit
	}
	if msg.String() == "ctrl+a" {
		m.toggleMode()
		return m, nil
	}

	if m.focus == 1 {
		switch msg.String() {
		case "esc":
			m.input.Blur()
			m.focus = 0
			return m, nil
		case "tab":
			m.input.Blur()
			if m.mode == 1 {
				m.focus = 2
				m.pathInput.Focus()
			} else {
				m.focus = 0
			}
			return m, nil
		case "enter":
			return m.submitFromInput()
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	if m.focus == 2 {
		switch msg.String() {
		case "esc":
			m.pathInput.Blur()
			m.focus = 0
			return m, nil
		case "tab", "enter":
			m.pathInput.Blur()
			m.focus = 1
			m.input.Focus()
			return m, nil
		}
		var cmd tea.Cmd
		m.pathInput, cmd = m.pathInput.Update(msg)
		return m, cmd
	}

	// Task list focused.
	switch msg.String() {
	case "q":
		if m.sseCancel != nil {
			m.sseCancel()
		}
		return m, tea.Quit
	case "i", "/", "enter":
		m.focus = 1
		m.input.Focus()
		return m, nil
	case "tab":
		if m.mode == 1 {
			m.focus = 2
			m.pathInput.Focus()
		} else {
			m.focus = 1
			m.input.Focus()
		}
		return m, nil
	case "a":
		m.toggleMode()
		return m, nil
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			return m, m.selectTask(m.tasks[m.cursor].ID)
		}
		return m, nil
	case "down", "j":
		if m.cursor < len(m.tasks)-1 {
			m.cursor++
			return m, m.selectTask(m.tasks[m.cursor].ID)
		}
		return m, nil
	case "t":
		m.tab = 1 - m.tab
		m.refreshViewport()
		if m.tab == 1 && m.reportID != m.selected {
			return m, m.fetchReport(m.selected)
		}
		return m, nil
	case "r":
		return m, tea.Batch(m.fetchTasks(), m.fetchHealth())
	case "g", "home":
		m.viewport.GotoTop()
		return m, nil
	case "G", "end":
		m.viewport.GotoBottom()
		return m, nil
	case "pgup", "pgdown":
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}
	return m, nil
}

// toggleMode switches between generate and analyze input modes.
func (m *Model) toggleMode() {
	if m.mode == 0 {
		m.mode = 1
		m.focus = 2
		m.input.Blur()
		m.pathInput.Focus()
	} else {
		m.mode = 0
		m.focus = 1
		m.pathInput.Blur()
		m.input.Focus()
	}
	m.layout()
}

// submitFromInput validates the current form and starts the task.
func (m *Model) submitFromInput() (tea.Model, tea.Cmd) {
	prompt := strings.TrimSpace(m.input.Value())
	if prompt == "" {
		m.errMsg = "prompt is required"
		return m, nil
	}
	if m.mode == 1 {
		path := strings.TrimSpace(m.pathInput.Value())
		if path == "" {
			m.errMsg = "RTL path is required in analyze mode"
			m.input.Blur()
			m.focus = 2
			m.pathInput.Focus()
			return m, nil
		}
		m.submitting = true
		m.errMsg = ""
		return m, m.submitAnalyze(prompt, path)
	}
	m.submitting = true
	m.errMsg = ""
	return m, m.submit(prompt)
}

// View implements tea.Model.
func (m *Model) View() string {
	if !m.ready {
		return "\n  " + m.spinner.View() + " connecting to " + m.addr + " …\n"
	}
	return lipgloss.JoinVertical(
		lipgloss.Left,
		m.renderHeader(),
		lipgloss.JoinHorizontal(lipgloss.Top, m.renderSidebar(), m.renderMain()),
		m.renderInput(),
		m.renderStatus(),
	)
}

func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	inputWidth := m.width - 6
	if inputWidth < 10 {
		inputWidth = 10
	}
	m.input.Width = inputWidth
	m.pathInput.Width = inputWidth

	bodyHeight := m.bodyHeight()
	viewportHeight := bodyHeight - 3
	if viewportHeight < 3 {
		viewportHeight = 3
	}
	m.viewport.Width = m.mainWidth() - 2
	m.viewport.Height = viewportHeight
	m.refreshViewport()
}

func (m *Model) refreshViewport() {
	follow := m.viewport.AtBottom()
	if m.tab == 0 {
		m.viewport.SetContent(m.renderLive())
	} else {
		m.viewport.SetContent(renderMarkdown(m.report))
	}
	if follow {
		m.viewport.GotoBottom()
	}
}

func (m *Model) renderHeader() string {
	dot := successStyle.Render("●")
	state := "connected"
	if !m.health {
		dot = dangerStyle.Render("●")
		state = "offline"
	}
	left := titleStyle.Render("◆ sv-multi-agent") + "  " + dot + " " + mutedStyle.Render(state)
	right := addrStyle.Render(m.addr)
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	return headerStyle.Render(left + strings.Repeat(" ", gap) + right)
}

func (m *Model) renderSidebar() string {
	var b strings.Builder
	b.WriteString(boldStyle.Render("Tasks") + " " + mutedStyle.Render(fmt.Sprintf("(%d)", len(m.tasks))) + "\n")

	if len(m.tasks) == 0 {
		b.WriteString(mutedStyle.Render("No tasks yet.\nSubmit one below."))
		return sidebarStyle.Width(m.sidebarWidth()).Render(b.String())
	}

	maxItems := m.bodyHeight() - 3
	if maxItems < 1 {
		maxItems = 1
	}
	start := 0
	if m.cursor >= maxItems {
		start = m.cursor - maxItems + 1
	}
	end := start + maxItems
	if end > len(m.tasks) {
		end = len(m.tasks)
	}

	for i := start; i < end; i++ {
		line := m.taskLine(m.tasks[i])
		if i == m.cursor {
			b.WriteString(selectedItemStyle.Width(m.sidebarWidth() - 2).Render(line))
		} else {
			b.WriteString(itemStyle.Render(line))
		}
		b.WriteString("\n")
	}
	if end < len(m.tasks) {
		b.WriteString(mutedStyle.Render(fmt.Sprintf("… %d more", len(m.tasks)-end)))
	}
	return sidebarStyle.Width(m.sidebarWidth()).Render(b.String())
}

func (m *Model) taskLine(task store.Task) string {
	id := task.ID
	if len(id) > 20 {
		id = id[:20]
	}
	stage := task.Stage
	if stage == "" {
		stage = task.Status
	}
	return boldStyle.Render(id) + "\n" + statusBadge(task.Status) + " " + mutedStyle.Render(stage)
}

func (m *Model) renderMain() string {
	tabs := m.renderTabs()
	return mainStyle.Width(m.mainWidth()).Render(tabs + "\n" + m.viewport.View())
}

func (m *Model) renderTabs() string {
	live := tabInactiveStyle.Render(" Live ")
	report := tabInactiveStyle.Render(" Report ")
	if m.tab == 0 {
		live = tabActiveStyle.Render(" Live ")
	} else {
		report = tabActiveStyle.Render(" Report ")
	}
	return live + mutedStyle.Render("│") + report
}

func (m *Model) renderInput() string {
	width := m.width - 6
	if width < 10 {
		width = 10
	}
	rows := make([]string, 0, 3)
	if m.mode == 1 {
		rows = append(rows, m.pathInput.View())
	}
	rows = append(rows, m.input.View())
	rows = append(rows, m.hint())
	return inputStyle.Width(width).Render(strings.Join(rows, "\n"))
}

func (m *Model) hint() string {
	if m.submitting {
		return accentStyle.Render(m.spinner.View() + " submitting…")
	}
	if m.errMsg != "" {
		return dangerStyle.Render("! " + m.errMsg)
	}
	if m.mode == 1 {
		return warnStyle.Render("ANALYZE") + mutedStyle.Render("  enter submit · tab next · ctrl+a generate · ctrl+c quit")
	}
	return mutedStyle.Render("GENERATE  enter submit · tab list · ctrl+a analyze · ctrl+c quit")
}

func (m *Model) renderStatus() string {
	items := []string{
		mutedStyle.Render("enter submit"),
		mutedStyle.Render("a analyze"),
		mutedStyle.Render("↑/↓ select"),
		mutedStyle.Render("tab focus"),
		mutedStyle.Render("t live/report"),
		mutedStyle.Render("r refresh"),
		mutedStyle.Render("q quit"),
	}
	return statusBarStyle.Render(strings.Join(items, "  ·  "))
}

func (m *Model) renderLive() string {
	if len(m.events) == 0 {
		if task, ok := m.selectedTask(); ok && len(task.Steps) > 0 {
			return m.renderSteps(task)
		}
		return mutedStyle.Render("No live events yet.\nType a prompt below and press Enter to submit a task.")
	}

	var b strings.Builder
	for i := 0; i < len(m.events); {
		ev := m.events[i]
		switch ev.Kind {
		case events.KindReasoning, events.KindContent:
			kind := ev.Kind
			var text strings.Builder
			for i < len(m.events) && m.events[i].Kind == kind {
				text.WriteString(m.events[i].Text)
				i++
			}
			style := reasoningStyle
			if kind == events.KindContent {
				style = contentStyle
			}
			b.WriteString(style.Render(strings.TrimRight(text.String(), "\n")))
			b.WriteString("\n\n")
		default:
			b.WriteString(renderEvent(ev))
			b.WriteString("\n")
			i++
		}
	}
	return b.String()
}

func renderEvent(ev events.Event) string {
	switch ev.Kind {
	case events.KindTaskStart:
		return accentStyle.Render("▶ task started") + "\n" + contentStyle.Render(ev.Text)
	case events.KindStepStart:
		return accentStyle.Render("◆ " + ev.Step + " …")
	case events.KindStepEnd:
		icon := successStyle.Render("✔")
		if ev.Status != "completed" {
			icon = dangerStyle.Render("✘")
		}
		line := icon + " " + boldStyle.Render(ev.Step) + " " + mutedStyle.Render(ev.Status)
		if strings.TrimSpace(ev.Text) != "" {
			line += "\n  " + mutedStyle.Render(ev.Text)
		}
		return line
	case events.KindArtifact:
		return successStyle.Render("+ " + ev.Text)
	case events.KindSimulator:
		return accentStyle.Render("~ " + ev.Text)
	case events.KindInfo:
		return mutedStyle.Render("ℹ " + ev.Text)
	case events.KindDone:
		if ev.Status == "completed" {
			return successStyle.Render("■ completed") + " " + mutedStyle.Render(ev.Text)
		}
		return dangerStyle.Render("■ "+ev.Status) + " " + mutedStyle.Render(ev.Text)
	default:
		return contentStyle.Render(ev.Text)
	}
}

func (m *Model) renderSteps(task store.Task) string {
	var b strings.Builder
	b.WriteString(boldStyle.Render("Steps") + "\n")
	for _, step := range task.Steps {
		icon := successStyle.Render("✔")
		switch step.Status {
		case "running":
			icon = warnStyle.Render("…")
		case "failed":
			icon = dangerStyle.Render("✘")
		}
		b.WriteString(icon + " " + boldStyle.Render(step.Name) + " " + mutedStyle.Render(step.Status))
		if strings.TrimSpace(step.Summary) != "" {
			b.WriteString("\n  " + mutedStyle.Render(step.Summary))
		}
		b.WriteString("\n")
	}
	if strings.TrimSpace(task.Summary) != "" {
		b.WriteString("\n" + contentStyle.Render(task.Summary))
	}
	return b.String()
}

func renderMarkdown(md string) string {
	if strings.TrimSpace(md) == "" {
		return mutedStyle.Render("No report yet. Run a task to completion, then open this tab.")
	}
	var b strings.Builder
	inCode := false
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			continue
		}
		if inCode {
			b.WriteString(mutedStyle.Render("  "+line) + "\n")
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "### "):
			b.WriteString(boldStyle.Render(strings.TrimPrefix(trimmed, "### ")) + "\n")
		case strings.HasPrefix(trimmed, "## "):
			b.WriteString(accentStyle.Render(strings.TrimPrefix(trimmed, "## ")) + "\n")
		case strings.HasPrefix(trimmed, "# "):
			b.WriteString(titleStyle.Render(strings.TrimPrefix(trimmed, "# ")) + "\n")
		case strings.HasPrefix(trimmed, "- "), strings.HasPrefix(trimmed, "* "):
			b.WriteString("  " + contentStyle.Render("• "+trimmed[2:]) + "\n")
		case trimmed == "":
			b.WriteString("\n")
		default:
			b.WriteString(contentStyle.Render(line) + "\n")
		}
	}
	return b.String()
}

func (m *Model) selectedTask() (store.Task, bool) {
	for _, task := range m.tasks {
		if task.ID == m.selected {
			return task, true
		}
	}
	return store.Task{}, false
}

func (m *Model) sidebarWidth() int {
	w := m.width / 4
	if w < 22 {
		w = 22
	}
	if w > 32 {
		w = 32
	}
	return w
}

func (m *Model) mainWidth() int {
	w := m.width - m.sidebarWidth() - 6
	if w < 20 {
		w = 20
	}
	return w
}

func (m *Model) bodyHeight() int {
	extra := 0
	if m.mode == 1 {
		extra = 1 // the RTL path row in analyze mode
	}
	h := m.height - 8 - extra
	if h < 6 {
		h = 6
	}
	return h
}

func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) fetchTasks() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tasks, err := client.ListTasks(ctx, 50)
		return tasksMsg{tasks: tasks, err: err}
	}
}

func (m *Model) fetchHealth() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return healthMsg{ok: client.Health(ctx)}
	}
}

func (m *Model) submit(prompt string) tea.Cmd {
	client := m.client
	iterations := m.maxIterations
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		id, err := client.Submit(ctx, prompt, iterations)
		if err != nil {
			return errMsg{err: err}
		}
		return submitMsg{taskID: id}
	}
}

func (m *Model) submitAnalyze(prompt, rtlPath string) tea.Cmd {
	client := m.client
	iterations := m.maxIterations
	return func() tea.Msg {
		files, err := collectSources(rtlPath, "rtl")
		if err != nil {
			return errMsg{err: err}
		}
		if err := checkBundleSize(files); err != nil {
			return errMsg{err: err}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		id, err := client.SubmitAnalyze(ctx, prompt, iterations, files)
		if err != nil {
			return errMsg{err: err}
		}
		return submitMsg{taskID: id}
	}
}

func (m *Model) fetchReport(id string) tea.Cmd {
	if id == "" {
		return nil
	}
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		text, err := client.Report(ctx, id)
		return reportMsg{taskID: id, text: text, err: err}
	}
}

func (m *Model) selectTask(id string) tea.Cmd {
	if id == "" || id == m.selected {
		return nil
	}
	m.selected = id
	m.events = nil
	m.report = ""
	m.reportID = ""
	m.tab = 0
	m.refreshViewport()
	return m.subscribe(id)
}

func (m *Model) subscribe(id string) tea.Cmd {
	if id == "" {
		return nil
	}
	if m.sseCancel != nil {
		m.sseCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.sseCancel = cancel
	channel := make(chan events.Event, 512)
	m.sseCh = channel
	m.subID = id
	go m.client.StreamEvents(ctx, id, channel)
	return waitEvent(id, channel)
}

func waitEvent(id string, channel <-chan events.Event) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-channel
		if !ok {
			return sseClosedMsg{taskID: id}
		}
		return sseEventMsg{taskID: id, event: event}
	}
}
