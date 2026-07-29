package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/key"
	list "charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	viewport "charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/session"
	"dv/internal/xdg"
)

const (
	tuiRefreshInterval = 30 * time.Second
	maxTUILogBytes     = 256 * 1024
)

type agentItem struct {
	name      string
	imageName string
	imageTag  string
	status    string
	age       string
	urls      []string
	selected  bool
}

func (a agentItem) Title() string {
	parts := []string{a.name}
	if a.status == "Running" {
		parts = append(parts, tuiForeground("42").Render("●"))
	}
	if a.selected {
		parts = append(parts, "✓")
	}
	return strings.Join(parts, "  ")
}

func (a agentItem) Description() string {
	parts := []string{a.status}
	if a.age != "" {
		parts = append(parts, a.age)
	}
	if len(a.urls) > 0 {
		parts = append(parts, strings.Join(a.urls, " "))
	}
	return strings.Join(parts, "  ")
}

func (a agentItem) FilterValue() string { return a.name + " " + a.status }

type agentDelegate struct{ dim bool }

func (agentDelegate) Height() int                         { return 1 }
func (agentDelegate) Spacing() int                        { return 0 }
func (agentDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

func (d agentDelegate) Render(w io.Writer, m list.Model, index int, raw list.Item) {
	item, ok := raw.(agentItem)
	if !ok {
		return
	}
	width := m.Width()
	if width <= 2 {
		return
	}
	selected := index == m.Index()
	prefix := "  "
	if selected {
		prefix = tuiForeground("245").Render("│ ")
	}

	nameStyle := tuiForeground("252")
	metaStyle := tuiForeground("243")
	if selected {
		nameStyle = tuiForeground("255").Bold(true)
		metaStyle = tuiForeground("249")
	}
	if d.dim {
		nameStyle = tuiForeground("245")
		metaStyle = tuiForeground("240")
	}

	markers := ""
	if item.status == "Running" {
		markers += "  " + tuiForeground("42").Render("●")
	}
	if item.selected {
		markers += "  " + tuiForeground("42").Render("✓")
	}
	available := width - 2
	var nameWidth, stateWidth, ageWidth int
	switch {
	case available >= 90:
		nameWidth, stateWidth, ageWidth = 28, 10, 14
	case available >= 55:
		nameWidth, stateWidth, ageWidth = 24, 10, available-24-10-4
	default:
		nameWidth, stateWidth = maxInt(12, available-12), 10
	}
	if nameWidth > available {
		nameWidth = available
	}
	markerWidth := lipgloss.Width(markers)
	plainNameWidth := maxInt(1, nameWidth-markerWidth)
	name := truncateToWidth(item.name, plainNameWidth) + markers

	var row strings.Builder
	row.WriteString(prefix)
	row.WriteString(nameStyle.Render(padToWidth(truncateToWidth(name, nameWidth), nameWidth)))
	remaining := available - nameWidth
	if remaining >= stateWidth+2 {
		row.WriteString("  ")
		row.WriteString(metaStyle.Render(padToWidth(truncateToWidth(item.status, stateWidth), stateWidth)))
		remaining -= stateWidth + 2
	}
	if ageWidth > 0 && remaining >= ageWidth+2 {
		row.WriteString("  ")
		row.WriteString(metaStyle.Render(padToWidth(truncateToWidth(item.age, ageWidth), ageWidth)))
		remaining -= ageWidth + 2
	}
	if remaining > 3 && len(item.urls) > 0 {
		row.WriteString("  ")
		row.WriteString(metaStyle.Render(truncateToWidth(item.urls[0], remaining-2)))
	}
	fmt.Fprint(w, ansi.Truncate(row.String(), width, ""))
}

func padToWidth(text string, width int) string {
	if padding := width - lipgloss.Width(text); padding > 0 {
		return text + strings.Repeat(" ", padding)
	}
	return text
}

type tuiAction string

const (
	actionNew         tuiAction = "new"
	actionStart       tuiAction = "start"
	actionStop        tuiAction = "stop"
	actionRemove      tuiAction = "remove"
	actionRename      tuiAction = "rename"
	actionSelect      tuiAction = "select"
	actionExtract     tuiAction = "extract"
	actionBuild       tuiAction = "build"
	actionSelectImage tuiAction = "select image"
)

type tuiActionRequest struct {
	action tuiAction
	target string
	value  string
	force  bool
}

type tuiInventory struct {
	cfg           config.Config
	imageName     string
	image         config.ImageConfig
	selectedAgent string
	agents        []agentItem
}

type tuiBackend interface {
	Load(context.Context) (tuiInventory, error)
	Sessions(context.Context, string) ([]docker.ExecSession, error)
	Run(context.Context, tuiActionRequest, io.Writer) error
}

type defaultTUIBackend struct{}

func (defaultTUIBackend) Load(ctx context.Context) (tuiInventory, error) {
	configDir, err := xdg.ConfigDir()
	if err != nil {
		return tuiInventory{}, err
	}
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		return tuiInventory{}, err
	}
	imageName, image, err := resolveImage(cfg, "")
	if err != nil {
		return tuiInventory{}, err
	}
	selected := currentAgentName(cfg)
	records, err := collectContainerInventory(ctx, cfg, imageName, image, selected, nil)
	if err != nil {
		return tuiInventory{}, err
	}
	agents := make([]agentItem, 0, len(records))
	for _, record := range records {
		agents = append(agents, agentItem{
			name:      record.name,
			imageName: record.imageName,
			imageTag:  record.imageTag,
			status:    record.status,
			age:       record.time,
			urls:      record.urls,
			selected:  record.selected,
		})
	}
	return tuiInventory{cfg: cfg, imageName: imageName, image: image, selectedAgent: selected, agents: agents}, nil
}

func (defaultTUIBackend) Sessions(ctx context.Context, name string) ([]docker.ExecSession, error) {
	return docker.ExecSessionsContext(ctx, name)
}

func (defaultTUIBackend) Run(ctx context.Context, request tuiActionRequest, output io.Writer) error {
	// Selection operations are small and safe to perform directly. This also
	// preserves terminal-local selection, which was lost when the TUI spawned a
	// child process whose session state died with it.
	switch request.action {
	case actionSelect:
		text, err := selectAgentDirect(request.target)
		_, _ = io.WriteString(output, text)
		return err
	case actionSelectImage:
		text, err := selectImageDirect(request.value)
		_, _ = io.WriteString(output, text)
		return err
	case actionStop:
		// The TUI performs its own active-session check and confirmation before
		// dispatch. Force here closes the race where a new session appears between
		// that check and runStop's second check, which cannot prompt in the TUI.
		return runStop(newTUIActionCommand(ctx, output, "stop"), request.target, true)
	case actionRename:
		return runRename(newTUIActionCommand(ctx, output, "rename"), request.target, request.value)
	case actionRemove:
		return runRemove(newTUIActionCommand(ctx, output, "remove"), []string{request.target}, request.force, true)
	case actionBuild:
		return runBuild(newTUIActionCommand(ctx, output, "build"), nil)
	case actionStart:
		return runDVSubprocess(ctx, output, "start", "--name", request.target)
	default:
		return fmt.Errorf("unsupported background TUI action %q", request.action)
	}
}

func runDVSubprocess(ctx context.Context, output io.Writer, args ...string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, executable, args...)
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func newTUIActionCommand(ctx context.Context, output io.Writer, name string) *cobra.Command {
	cmd := &cobra.Command{Use: name}
	cmd.SetContext(ctx)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(output)
	cmd.SetErr(output)
	return cmd
}

func selectAgentDirect(name string) (string, error) {
	configDir, err := xdg.ConfigDir()
	if err != nil {
		return "", err
	}
	if err := session.SetCurrentAgent(name); err != nil {
		return "", fmt.Errorf("could not save session state: %w", err)
	}
	if err := config.Update(configDir, func(cfg *config.Config) error {
		cfg.SelectedAgent = name
		return nil
	}); err != nil {
		return "", err
	}
	return fmt.Sprintf("Selected agent: %s\n", name), nil
}

func selectImageDirect(name string) (string, error) {
	configDir, err := xdg.ConfigDir()
	if err != nil {
		return "", err
	}
	if err := config.Update(configDir, func(cfg *config.Config) error {
		if _, ok := cfg.Images[name]; !ok {
			return fmt.Errorf("unknown image %q", name)
		}
		cfg.SelectedImage = name
		return nil
	}); err != nil {
		return "", err
	}
	return fmt.Sprintf("Selected image: %s\n", name), nil
}

type keyMap struct {
	New, Start, Stop, Remove, Rename, Select, Enter, Extract, Build, Images key.Binding
	Refresh, Logs, Focus, Help, Cancel, Quit                                key.Binding
}

func newKeyMap() keyMap {
	return keyMap{
		New:     key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new")),
		Start:   key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "start")),
		Stop:    key.NewBinding(key.WithKeys("S"), key.WithHelp("S", "stop")),
		Remove:  key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "remove")),
		Rename:  key.NewBinding(key.WithKeys("f2"), key.WithHelp("F2", "rename")),
		Select:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "select")),
		Enter:   key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "shell")),
		Extract: key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "extract")),
		Build:   key.NewBinding(key.WithKeys("b"), key.WithHelp("b", "build")),
		Images:  key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "next image")),
		Refresh: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Logs:    key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "toggle logs")),
		Focus:   key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "focus")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Cancel:  key.NewBinding(key.WithKeys("esc", "c"), key.WithHelp("esc", "cancel")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

func (k keyMap) shortHelp() []key.Binding {
	return []key.Binding{k.New, k.Start, k.Stop, k.Remove, k.Select, k.Enter, k.Logs, k.Refresh, k.Help, k.Quit}
}

func (k keyMap) fullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.New, k.Start, k.Stop, k.Remove, k.Rename},
		{k.Select, k.Enter, k.Extract, k.Build, k.Images},
		{k.Logs, k.Refresh, k.Focus, k.Cancel, k.Help, k.Quit},
	}
}

type tuiFocus int

const (
	focusAgents tuiFocus = iota
	focusLogs
)

type tuiModal int

const (
	modalNone tuiModal = iota
	modalHelp
	modalRename
	modalConfirm
)

type model struct {
	ctx     context.Context
	backend tuiBackend
	keys    keyMap

	list        list.Model
	logVP       viewport.Model
	renameInput textinput.Model
	spinner     spinner.Model

	inventory tuiInventory
	focus     tuiFocus
	modal     tuiModal
	showLogs  bool
	confirm   tuiActionRequest
	sessions  []docker.ExecSession

	width, height  int
	loading        bool
	busy           bool
	operation      string
	currentAction  tuiAction
	operationID    uint64
	operationMsgs  chan tea.Msg
	loadID         uint64
	cancel         context.CancelFunc
	quitAfterOp    bool
	pendingEnter   string
	sessionWarning string

	logText   string
	toast     string
	toastID   uint64
	errText   string
	lastError error
}

func initialModel(cmd *cobra.Command) model {
	ctx := context.Background()
	if cmd != nil && cmd.Context() != nil {
		ctx = cmd.Context()
	}
	return newTUIModel(ctx, defaultTUIBackend{})
}

func newTUIModel(ctx context.Context, backend tuiBackend) model {
	if ctx == nil {
		ctx = context.Background()
	}
	if backend == nil {
		backend = defaultTUIBackend{}
	}
	m := model{ctx: ctx, backend: backend, keys: newKeyMap(), loading: true, loadID: 1}
	m.list = list.New(nil, agentDelegate{}, 0, 0)
	m.list.SetShowTitle(false)
	m.list.SetShowHelp(false)
	m.list.SetShowStatusBar(false)
	m.list.SetFilteringEnabled(true)
	m.logVP = viewport.New(viewport.WithWidth(0), viewport.WithHeight(0))
	m.renameInput = textinput.New()
	m.renameInput.Prompt = "New name: "
	m.renameInput.CharLimit = 100
	m.renameInput.Placeholder = "agent-new-name"
	m.spinner = spinner.New()
	if os.Getenv("NO_COLOR") == "" {
		m.spinner.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	}
	if w, h, ok := measureTerminal(); ok {
		m.resize(w, h)
	}
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.loadCmd(), refreshTickCmd())
}

func (m model) View() tea.View {
	view := tea.NewView(m.viewString())
	view.AltScreen = true
	return view
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if tick, ok := msg.(spinner.TickMsg); ok {
		if !m.loading && !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(tick)
		return m, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case inventoryMsg:
		if msg.id != m.loadID {
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			m.lastError = msg.err
			m.errText = msg.err.Error()
			return m, nil
		}
		m.lastError = nil
		m.errText = ""
		m.applyInventory(msg.inventory)
		return m, nil
	case sessionCheckMsg:
		if msg.id != m.operationID {
			return m, nil
		}
		m.busy = false
		m.operation = ""
		m.cancel = nil
		if msg.err != nil {
			m.sessionWarning = fmt.Sprintf("Could not enumerate active sessions: %v", msg.err)
			m.confirm = msg.request
			m.sessions = nil
			m.modal = modalConfirm
			return m, nil
		}
		m.sessionWarning = ""
		m.sessions = msg.sessions
		if msg.request.action == actionRemove || len(msg.sessions) > 0 {
			m.confirm = msg.request
			m.modal = modalConfirm
			return m, nil
		}
		msg.request.force = true // session safety was already checked by the TUI
		return m.startOperation(msg.request)
	case interactiveDoneMsg:
		if msg.id != m.operationID {
			return m, nil
		}
		m.busy = false
		m.operation = ""
		m.currentAction = ""
		m.cancel = nil
		if msg.output != "" {
			m.appendLog(msg.output)
		}
		if msg.err != nil {
			m.appendLogHeader("error: " + msg.err.Error())
			m.errText = msg.err.Error()
			m.setToast(fmt.Sprintf("%s failed", msg.request.action))
			m.loading = true
			return m, tea.Batch(m.toastCmd(), m.nextLoadCmd(), m.spinner.Tick)
		}
		m.errText = ""
		m.setToast(fmt.Sprintf("%s complete", msg.request.action))
		m.loading = true
		return m, tea.Batch(m.toastCmd(), m.nextLoadCmd(), m.spinner.Tick)
	case operationProgressMsg:
		if msg.id != m.operationID {
			return m, nil
		}
		m.appendLog(msg.text)
		return m, m.waitOperationCmd()
	case operationDoneMsg:
		if msg.id != m.operationID {
			return m, nil
		}
		m.busy = false
		m.operation = ""
		m.currentAction = ""
		m.cancel = nil
		m.operationMsgs = nil
		quitAfter := m.quitAfterOp
		m.quitAfterOp = false
		if msg.err != nil {
			m.appendLogHeader("error: " + msg.err.Error())
			var partial *partialOperationError
			switch {
			case errors.As(msg.err, &partial):
				m.errText = partial.Error()
				m.setToast(fmt.Sprintf("%s complete with warning", msg.request.action))
			case errors.Is(msg.err, context.Canceled):
				m.setToast("Operation canceled")
			default:
				m.errText = msg.err.Error()
				m.setToast(fmt.Sprintf("%s failed", msg.request.action))
			}
			if quitAfter {
				return m, tea.Quit
			}
			m.loading = true
			return m, tea.Batch(m.toastCmd(), m.nextLoadCmd(), m.spinner.Tick)
		}
		m.errText = ""
		m.setToast(fmt.Sprintf("%s complete", msg.request.action))
		if quitAfter {
			return m, tea.Quit
		}
		m.loading = true
		return m, tea.Batch(m.toastCmd(), m.nextLoadCmd(), m.spinner.Tick)
	case toastExpiredMsg:
		if msg.id == m.toastID {
			m.toast = ""
		}
		return m, nil
	case refreshTickMsg:
		cmds := []tea.Cmd{refreshTickCmd()}
		if !m.busy && m.modal == modalNone && m.list.FilterState() == list.Unfiltered {
			// Background refreshes are intentionally silent. A spinner and title-bar
			// churn every few seconds makes an otherwise idle TUI appear to flash.
			cmds = append(cmds, m.nextLoadCmd())
		}
		return m, tea.Batch(cmds...)
	}

	if m.focus == focusLogs {
		var cmd tea.Cmd
		m.logVP, cmd = m.logVP.Update(msg)
		return m, cmd
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		if !m.busy {
			return m, tea.Quit
		}
		if m.operation == "checking active sessions" {
			m.cancelOperation()
			return m, tea.Quit
		}
		m.quitAfterOp = true
		if m.operationCancelable() {
			m.requestOperationCancel()
		}
		m.setToast("Waiting for " + string(m.currentAction) + " to finish before quitting")
		return m, m.toastCmd()
	}

	if m.modal != modalNone {
		return m.handleModalKey(msg)
	}

	// Printable keys belong exclusively to the list while its filter editor is open.
	if m.list.FilterState() == list.Filtering {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}

	if m.busy {
		switch msg.String() {
		case "esc", "c":
			if m.operation == "checking active sessions" {
				m.cancelOperation()
				m.setToast("Operation canceled")
				return m, m.toastCmd()
			}
			if !m.operationCancelable() {
				m.setToast(m.operation + " cannot be canceled safely")
				return m, m.toastCmd()
			}
			m.requestOperationCancel()
			m.setToast("Canceling " + string(m.currentAction) + "…")
			return m, m.toastCmd()
		case "q":
			m.quitAfterOp = true
			if m.operationCancelable() {
				m.requestOperationCancel()
			}
			m.setToast("Waiting for " + string(m.currentAction) + " to finish before quitting")
			return m, m.toastCmd()
		default:
			return m, nil
		}
	}

	if m.focus == focusLogs {
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, m.keys.Help):
			m.modal = modalHelp
			return m, nil
		case key.Matches(msg, m.keys.Logs):
			m.toggleLogs()
			return m, nil
		case key.Matches(msg, m.keys.Focus):
			m.focus = focusAgents
			return m, nil
		case key.Matches(msg, m.keys.Refresh):
			m.loading = true
			return m, tea.Batch(m.nextLoadCmd(), m.spinner.Tick)
		default:
			var cmd tea.Cmd
			m.logVP, cmd = m.logVP.Update(msg)
			return m, cmd
		}
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, m.keys.Help):
		m.modal = modalHelp
		return m, nil
	case key.Matches(msg, m.keys.Logs):
		m.toggleLogs()
		return m, nil
	case key.Matches(msg, m.keys.Focus):
		if m.focus == focusAgents && m.logsVisible() {
			m.focus = focusLogs
		} else {
			m.focus = focusAgents
		}
		return m, nil
	case key.Matches(msg, m.keys.Refresh):
		m.loading = true
		return m, tea.Batch(m.nextLoadCmd(), m.spinner.Tick)
	case key.Matches(msg, m.keys.Rename):
		if agent := m.selectedAgent(); agent != "" {
			m.modal = modalRename
			m.renameInput.SetValue(agent)
			m.renameInput.CursorEnd()
			return m, m.renameInput.Focus()
		}
	case key.Matches(msg, m.keys.New):
		return m.startInteractiveOperation(tuiActionRequest{action: actionNew, target: autogenName()})
	case key.Matches(msg, m.keys.Start):
		return m.startSelected(actionStart)
	case key.Matches(msg, m.keys.Stop):
		return m.checkSessions(tuiActionRequest{action: actionStop, target: m.selectedAgent()})
	case key.Matches(msg, m.keys.Remove):
		item, ok := m.selectedAgentItem()
		if !ok {
			m.errText = "no agent selected"
			return m, nil
		}
		request := tuiActionRequest{action: actionRemove, target: item.name}
		if item.status != "Running" {
			m.confirm = request
			m.sessions = nil
			m.modal = modalConfirm
			return m, nil
		}
		return m.checkSessions(request)
	case key.Matches(msg, m.keys.Select):
		return m.startSelected(actionSelect)
	case key.Matches(msg, m.keys.Enter):
		if agent := m.selectedAgent(); agent != "" {
			m.pendingEnter = agent
			return m, tea.Quit
		}
	case key.Matches(msg, m.keys.Extract):
		if target := m.selectedAgent(); target != "" {
			return m.startInteractiveOperation(tuiActionRequest{action: actionExtract, target: target})
		}
		m.errText = "no agent selected"
		return m, nil
	case key.Matches(msg, m.keys.Build):
		return m.startOperation(tuiActionRequest{action: actionBuild})
	case key.Matches(msg, m.keys.Images):
		if next := m.nextImage(); next != "" {
			return m.startOperation(tuiActionRequest{action: actionSelectImage, value: next})
		}
	}

	if m.focus == focusLogs {
		var cmd tea.Cmd
		m.logVP, cmd = m.logVP.Update(msg)
		return m, cmd
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m model) handleModalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.modal {
	case modalHelp:
		switch msg.String() {
		case "?", "esc", "q", "enter":
			m.modal = modalNone
		}
		return m, nil
	case modalRename:
		switch msg.String() {
		case "esc":
			m.modal = modalNone
			m.renameInput.Blur()
			return m, nil
		case "enter":
			oldName := m.selectedAgent()
			newName := strings.TrimSpace(m.renameInput.Value())
			m.modal = modalNone
			m.renameInput.Blur()
			if newName == "" || newName == oldName {
				return m, nil
			}
			return m.startOperation(tuiActionRequest{action: actionRename, target: oldName, value: newName})
		default:
			var cmd tea.Cmd
			m.renameInput, cmd = m.renameInput.Update(msg)
			return m, cmd
		}
	case modalConfirm:
		switch strings.ToLower(msg.String()) {
		case "y", "enter":
			request := m.confirm
			request.force = true
			m.modal = modalNone
			m.sessions = nil
			m.sessionWarning = ""
			return m.startOperation(request)
		case "n", "esc", "q":
			m.modal = modalNone
			m.sessions = nil
			m.sessionWarning = ""
			m.currentAction = ""
			m.confirm = tuiActionRequest{}
			return m, nil
		}
		return m, nil
	}
	return m, nil
}

func (m model) operationCancelable() bool {
	return m.currentAction != actionRemove && m.currentAction != actionRename
}

func (m *model) requestOperationCancel() {
	if m.cancel != nil {
		m.cancel()
	}
	m.operation = "canceling " + string(m.currentAction)
}

func (m *model) cancelOperation() {
	if m.cancel != nil {
		m.cancel()
	}
	// Invalidate all queued progress/session messages before releasing the UI.
	m.operationID++
	m.busy = false
	m.operation = ""
	m.currentAction = ""
	m.operationMsgs = nil
	m.cancel = nil
	m.sessionWarning = ""
}

func (m model) startSelected(action tuiAction) (tea.Model, tea.Cmd) {
	target := m.selectedAgent()
	if target == "" {
		m.errText = "no agent selected"
		return m, nil
	}
	return m.startOperation(tuiActionRequest{action: action, target: target})
}

func (m model) checkSessions(request tuiActionRequest) (tea.Model, tea.Cmd) {
	if request.target == "" {
		m.errText = "no agent selected"
		return m, nil
	}
	m.operationID++
	id := m.operationID
	m.busy = true
	m.currentAction = request.action
	m.operation = "checking active sessions"
	m.errText = ""
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	m.cancel = cancel
	return m, tea.Batch(m.spinner.Tick, func() tea.Msg {
		sessions, err := m.backend.Sessions(ctx, request.target)
		return sessionCheckMsg{id: id, request: request, sessions: sessions, err: err}
	})
}

func (m model) startInteractiveOperation(request tuiActionRequest) (tea.Model, tea.Cmd) {
	m.operationID++
	id := m.operationID
	m.busy = true
	m.currentAction = request.action
	m.operation = string(request.action)
	m.errText = ""
	target := strings.TrimSpace(strings.TrimSpace(request.target + " " + request.value))
	header := "$ " + string(request.action)
	if target != "" {
		header += " " + target
	}
	m.appendLogHeader(header)

	executor := &tuiCobraExec{ctx: m.ctx, request: request}
	return m, tea.Exec(executor, func(err error) tea.Msg {
		return interactiveDoneMsg{id: id, request: request, output: executor.output.String(), err: err}
	})
}

type synchronizedOutput struct {
	mu sync.Mutex
	b  strings.Builder
}

func (o *synchronizedOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.Write(data)
}

func (o *synchronizedOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}

type tuiCobraExec struct {
	ctx     context.Context
	request tuiActionRequest
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	output  synchronizedOutput
}

func (e *tuiCobraExec) SetStdin(reader io.Reader)  { e.stdin = reader }
func (e *tuiCobraExec) SetStdout(writer io.Writer) { e.stdout = io.MultiWriter(writer, &e.output) }
func (e *tuiCobraExec) SetStderr(writer io.Writer) { e.stderr = io.MultiWriter(writer, &e.output) }

func (e *tuiCobraExec) Run() error {
	var command *cobra.Command
	var args []string
	switch e.request.action {
	case actionNew:
		command, args = newCmd, []string{e.request.target}
	case actionExtract:
		command = extractCmd
		nameFlag := command.Flags().Lookup("name")
		oldValue, oldChanged := nameFlag.Value.String(), nameFlag.Changed
		if err := command.Flags().Set("name", e.request.target); err != nil {
			return err
		}
		defer func() {
			_ = command.Flags().Set("name", oldValue)
			nameFlag.Changed = oldChanged
		}()
	default:
		return fmt.Errorf("unsupported interactive TUI action %q", e.request.action)
	}
	oldContext := command.Context()
	oldIn := command.InOrStdin()
	oldOut := command.OutOrStdout()
	oldErr := command.ErrOrStderr()
	defer func() {
		command.SetContext(oldContext)
		command.SetIn(oldIn)
		command.SetOut(oldOut)
		command.SetErr(oldErr)
	}()
	command.SetContext(e.ctx)
	command.SetIn(e.stdin)
	command.SetOut(e.stdout)
	command.SetErr(e.stderr)
	return command.RunE(command, args)
}

func (m model) startOperation(request tuiActionRequest) (tea.Model, tea.Cmd) {
	m.operationID++
	id := m.operationID
	m.busy = true
	m.currentAction = request.action
	m.operation = string(request.action)
	m.errText = ""
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	m.operationMsgs = make(chan tea.Msg, 128)

	target := strings.TrimSpace(strings.TrimSpace(request.target + " " + request.value))
	header := "$ " + string(request.action)
	if target != "" {
		header += " " + target
	}
	m.appendLogHeader(header)

	backend := m.backend
	events := m.operationMsgs
	start := func() tea.Msg {
		go func() {
			writer := &operationProgressWriter{ctx: ctx, id: id, events: events}
			err := backend.Run(ctx, request, writer)
			if flushErr := writer.Flush(); err == nil && flushErr != nil {
				err = flushErr
			}
			if err == nil && ctx.Err() != nil {
				err = ctx.Err()
			}
			events <- operationDoneMsg{id: id, request: request, err: err}
		}()
		return <-events
	}
	return m, tea.Batch(m.spinner.Tick, start)
}

func (m model) waitOperationCmd() tea.Cmd {
	events := m.operationMsgs
	if events == nil {
		return nil
	}
	return func() tea.Msg { return <-events }
}

func (m model) loadCmd() tea.Cmd {
	ctx := m.ctx
	backend := m.backend
	id := m.loadID
	return func() tea.Msg {
		loadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		inventory, err := backend.Load(loadCtx)
		return inventoryMsg{id: id, inventory: inventory, err: err}
	}
}

func (m *model) nextLoadCmd() tea.Cmd {
	m.loadID++
	return m.loadCmd()
}

func refreshTickCmd() tea.Cmd {
	return tea.Tick(tuiRefreshInterval, func(time.Time) tea.Msg { return refreshTickMsg{} })
}

func (m *model) applyInventory(inventory tuiInventory) {
	previous := m.selectedAgent()
	m.inventory = inventory
	items := make([]list.Item, len(inventory.agents))
	for i := range inventory.agents {
		items[i] = inventory.agents[i]
	}
	if filterCmd := m.list.SetItems(items); filterCmd != nil {
		// Bubbles filters asynchronously by default. Inventory application already
		// runs on the event loop, so resolve this pure in-memory filter immediately
		// before restoring the cursor against VisibleItems.
		filtered, _ := m.list.Update(filterCmd())
		m.list = filtered
	}
	wanted := previous
	if wanted == "" {
		wanted = inventory.selectedAgent
	}
	for i, visible := range m.list.VisibleItems() {
		if item, ok := visible.(agentItem); ok && item.name == wanted {
			m.list.Select(i)
			break
		}
	}
	m.resize(m.width, m.height)
}

func (m model) selectedAgentItem() (agentItem, bool) {
	item, ok := m.list.SelectedItem().(agentItem)
	return item, ok
}

func (m model) selectedAgent() string {
	if item, ok := m.selectedAgentItem(); ok {
		return item.name
	}
	return ""
}

func (m model) nextImage() string {
	names := make([]string, 0, len(m.inventory.cfg.Images))
	for name := range m.inventory.cfg.Images {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) < 2 {
		return ""
	}
	for i, name := range names {
		if name == m.inventory.imageName {
			return names[(i+1)%len(names)]
		}
	}
	return names[0]
}

func (m *model) appendLogHeader(header string) {
	if m.logText != "" && !strings.HasSuffix(m.logText, "\n") {
		m.appendLog("\n")
	}
	m.appendLog(header + "\n")
}

func (m *model) appendLog(text string) {
	if text == "" {
		return
	}
	m.logText += text
	if len(m.logText) > maxTUILogBytes {
		cut := len(m.logText) - maxTUILogBytes
		if newline := strings.IndexByte(m.logText[cut:], '\n'); newline >= 0 {
			cut += newline + 1
		} else {
			for cut < len(m.logText) && m.logText[cut]&0xc0 == 0x80 {
				cut++
			}
		}
		m.logText = "… older output discarded …\n" + m.logText[cut:]
	}
	m.logVP.SetContent(m.logText)
	m.logVP.GotoBottom()
}

func (m *model) setToast(text string) {
	m.toast = text
	m.toastID++
}

func (m model) toastCmd() tea.Cmd {
	id := m.toastID
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return toastExpiredMsg{id: id} })
}

type tuiLayoutTier int

const (
	layoutNarrow tuiLayoutTier = iota
	layoutCompact
	layoutWide
)

func layoutTierForWidth(width int) tuiLayoutTier {
	switch {
	case width >= 90:
		return layoutWide
	case width >= 70:
		return layoutCompact
	default:
		return layoutNarrow
	}
}

func (m *model) resize(width, height int) {
	if width <= 0 || height <= 0 {
		return
	}
	m.width, m.height = width, height
	reservedRows := 4 // status, message, footer, separators
	if height < 16 {
		reservedRows = 3 // transient messages replace the footer in short panes
	}
	bodyHeight := height - reservedRows
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	logHeight := 0
	if m.showLogs && height >= 20 {
		logHeight = bodyHeight / 3
		if logHeight < 5 {
			logHeight = 5
		}
		if logHeight > 12 {
			logHeight = 12
		}
	}
	listHeight := bodyHeight - logHeight
	if logHeight > 0 {
		listHeight--
	}
	if listHeight < 1 {
		listHeight = 1
	}
	if listHeight > height-2 {
		listHeight = maxInt(1, height-2)
	}

	listWidth := width
	if layoutTierForWidth(width) == layoutWide {
		detailWidth := width / 3
		if detailWidth < 36 {
			detailWidth = 36
		}
		if detailWidth > 60 {
			detailWidth = 60
		}
		listWidth = width - detailWidth - 1
	}
	m.list.SetSize(maxInt(1, minInt(width, listWidth)), listHeight)
	m.logVP.SetWidth(maxInt(1, width))
	m.logVP.SetHeight(logHeight)
}

func (m *model) toggleLogs() {
	m.showLogs = !m.showLogs
	if !m.showLogs {
		m.focus = focusAgents
	}
	m.resize(m.width, m.height)
}

func (m model) logsVisible() bool { return m.showLogs && m.logVP.Height() > 0 }

func (m model) viewString() string {
	width := m.currentWidth()
	var body strings.Builder
	body.WriteString(m.renderStatus(width))
	body.WriteByte('\n')

	agentList := m.list
	agentList.SetDelegate(agentDelegate{dim: m.focus == focusLogs})
	listView := agentList.View()
	if len(m.inventory.agents) == 0 && !m.loading {
		listView = m.renderEmpty(m.list.Width())
	}
	if layoutTierForWidth(width) == layoutWide {
		detailWidth := width - m.list.Width()
		detail := m.renderDetail(detailWidth, m.list.Height())
		body.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, listView, detail))
	} else {
		body.WriteString(listView)
	}

	if m.logsVisible() {
		body.WriteByte('\n')
		label := "logs"
		if m.focus == focusLogs {
			label = "logs [focused]"
		}
		body.WriteString(truncateToWidth("─ "+label+" "+strings.Repeat("─", width), width))
		body.WriteByte('\n')
		body.WriteString(m.logVP.View())
	}
	body.WriteByte('\n')
	message := m.renderMessage(width)
	if m.height >= 16 {
		body.WriteString(message)
		body.WriteByte('\n')
		body.WriteString(m.renderFooter(width))
	} else if message != "" {
		body.WriteString(message)
	} else {
		body.WriteString(m.renderFooter(width))
	}

	content := body.String()
	if m.modal != modalNone {
		content = overlayModal(content, m.renderModal(), width, m.currentHeight())
	}
	return content
}

func (m model) renderStatus(width int) string {
	if width <= 0 {
		return ""
	}
	tier := layoutTierForWidth(width)
	left := m.inventory.imageName
	if tier != layoutNarrow && left != "" {
		left = "image: " + left
	}
	if selected := m.inventory.selectedAgent; selected != "" {
		label := "selected: " + selected
		if tier == layoutNarrow {
			label = "sel: " + selected
		}
		if left != "" {
			left += "  "
		}
		left += label
	}

	right := ""
	if m.loading || m.busy {
		label := m.operation
		if label == "" {
			label = "refreshing"
		}
		right = m.spinner.View() + " " + label
	} else if tier != layoutNarrow {
		right = fmt.Sprintf("%d agents", len(m.inventory.agents))
	}
	innerWidth := maxInt(1, width-2)
	left = truncateToWidth(left, innerWidth)
	if right != "" && lipgloss.Width(left)+lipgloss.Width(right)+2 <= innerWidth {
		left += strings.Repeat(" ", innerWidth-lipgloss.Width(left)-lipgloss.Width(right)) + right
	}
	line := " " + padToWidth(left, innerWidth) + " "
	style := tuiForeground("249")
	if os.Getenv("NO_COLOR") == "" {
		style = style.Background(lipgloss.Color("236"))
	}
	return style.Render(ansi.Truncate(line, width, ""))
}

func (m model) renderEmpty(width int) string {
	message := "No agents for this image. Press n to create one."
	if m.lastError != nil {
		message = "Could not load agents\n\n" + m.lastError.Error() + "\n\nCheck that Docker is running, then press r to retry."
	}
	return lipgloss.NewStyle().Width(maxInt(1, width-2)).Padding(1, 2).Render(message)
}

func (m model) renderDetail(width, height int) string {
	item, ok := m.list.SelectedItem().(agentItem)
	if !ok {
		return tuiDividerStyle(width, height).Render("Select an agent to see details.")
	}
	var b strings.Builder
	b.WriteString(tuiForeground("252").Bold(true).Render("Details"))
	b.WriteString("\n\n")
	b.WriteString(renderDetailLine("Name", item.name))
	b.WriteString(renderDetailLine("State", item.status))
	if item.age != "" {
		b.WriteString(renderDetailLine("Age", item.age))
	}
	b.WriteString(renderDetailLine("Image", item.imageName))
	b.WriteString(renderDetailLine("Tag", item.imageTag))
	if len(item.urls) > 0 {
		b.WriteString(renderDetailLine("URL", strings.Join(item.urls, " ")))
	}
	return tuiDividerStyle(width, height).Render(b.String())
}

func renderDetailLine(label, value string) string {
	return tuiForeground("245").Render(fmt.Sprintf("%-8s", label+":")) + tuiForeground("252").Render(value) + "\n"
}

func tuiDividerStyle(width, height int) lipgloss.Style {
	style := lipgloss.NewStyle().
		Width(maxInt(1, width-1)).
		Height(maxInt(1, height-1)).
		PaddingLeft(1).
		Border(lipgloss.NormalBorder(), false, false, false, true)
	if os.Getenv("NO_COLOR") == "" {
		style = style.BorderForeground(lipgloss.Color("240"))
	}
	return style
}

func (m model) renderMessage(width int) string {
	message := ""
	style := tuiForeground("245")
	if m.errText != "" {
		message = "Error: " + m.errText
		style = tuiForeground("203")
	} else if m.toast != "" {
		message = m.toast
		style = tuiForeground("42")
	} else if m.showLogs && !m.logsVisible() {
		message = "Logs need a taller terminal. Press l to hide them."
	}
	return style.Render(truncateToWidth(message, width))
}

func (m model) renderFooter(width int) string {
	var footer string
	if m.focus == focusLogs {
		footer = "↑/↓ scroll  │  l hide  │  tab agents  │  ? help  │  q quit"
	} else {
		var bindings []key.Binding
		switch layoutTierForWidth(width) {
		case layoutNarrow:
			bindings = []key.Binding{m.keys.New, m.keys.Start, m.keys.Stop, m.keys.Select, m.keys.Help, m.keys.Quit}
		case layoutCompact:
			bindings = []key.Binding{m.keys.New, m.keys.Start, m.keys.Stop, m.keys.Remove, m.keys.Select, m.keys.Logs, m.keys.Help, m.keys.Quit}
		default:
			bindings = m.keys.shortHelp()
		}
		parts := make([]string, 0, len(bindings))
		for _, binding := range bindings {
			help := binding.Help()
			parts = append(parts, help.Key+" "+help.Desc)
		}
		footer = strings.Join(parts, "  │  ")
	}
	return tuiForeground("245").Render(truncateToWidth(footer, width))
}

func overlayModal(base, modal string, width, height int) string {
	if width <= 0 || height <= 0 {
		return modal
	}
	base = lipgloss.NewStyle().Faint(true).Render(base)
	baseLines := strings.Split(base, "\n")
	for len(baseLines) < height {
		baseLines = append(baseLines, "")
	}
	modalLines := strings.Split(modal, "\n")
	modalWidth := lipgloss.Width(modal)
	modalHeight := len(modalLines)
	x := maxInt(0, (width-modalWidth)/2)
	y := maxInt(1, (height-modalHeight)/2)
	for i, modalLine := range modalLines {
		row := y + i
		if row >= height {
			break
		}
		baseLine := padToWidth(ansi.Truncate(baseLines[row], width, ""), width)
		left := ansi.Cut(baseLine, 0, x)
		rightStart := minInt(width, x+modalWidth)
		right := ansi.Cut(baseLine, rightStart, width)
		baseLines[row] = padToWidth(left, x) + padToWidth(modalLine, modalWidth) + right
	}
	return strings.Join(baseLines[:height], "\n")
}

func (m model) renderModal() string {
	modalWidth := minInt(maxInt(10, m.currentWidth()-4), 90)
	var content string
	switch m.modal {
	case modalHelp:
		var b strings.Builder
		b.WriteString("dv shortcuts\n\n")
		for _, row := range m.keys.fullHelp() {
			parts := make([]string, 0, len(row))
			for _, binding := range row {
				help := binding.Help()
				parts = append(parts, help.Key+" "+help.Desc)
			}
			b.WriteString(strings.Join(parts, "    "))
			b.WriteByte('\n')
		}
		b.WriteString("\n/ filters agents. j/k and arrows navigate. l toggles logs. Esc closes this help.")
		content = b.String()
	case modalRename:
		content = "Rename agent\n\n" + m.renameInput.View() + "\n\nEnter confirms • Esc cancels"
	case modalConfirm:
		verb := string(m.confirm.action)
		if verb != "" {
			verb = strings.ToUpper(verb[:1]) + verb[1:]
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s %s?\n\n", verb, m.confirm.target)
		if m.sessionWarning != "" {
			b.WriteString(tuiForeground("203").Render(m.sessionWarning))
			b.WriteString("\nContinuing may interrupt unknown sessions.\n\n")
		}
		if len(m.sessions) > 0 {
			fmt.Fprintf(&b, "%d active session(s) will be interrupted:\n", len(m.sessions))
			for _, active := range m.sessions {
				fmt.Fprintf(&b, "  • %s\n", truncateToWidth(active.Command, modalWidth-8))
			}
			b.WriteByte('\n')
		}
		if m.confirm.action == actionRemove {
			b.WriteString("This permanently removes the development container.\n\n")
		}
		b.WriteString("y/Enter confirm • n/Esc cancel")
		content = b.String()
	}
	style := lipgloss.NewStyle().Width(modalWidth).Padding(1, 2).Border(lipgloss.RoundedBorder())
	if os.Getenv("NO_COLOR") == "" {
		borderColor := lipgloss.Color("245")
		if m.modal == modalConfirm && m.confirm.action == actionRemove {
			borderColor = lipgloss.Color("203")
		}
		style = style.BorderForeground(borderColor)
	}
	return style.Render(content)
}

func truncateToWidth(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(text) <= width {
		return text
	}
	target := width - 1
	var b strings.Builder
	used := 0
	for _, r := range text {
		runeWidth := lipgloss.Width(string(r))
		if used+runeWidth > target {
			break
		}
		b.WriteRune(r)
		used += runeWidth
	}
	return b.String() + "…"
}

func (m model) currentWidth() int {
	if m.width > 0 {
		return m.width
	}
	return 80
}

func (m model) currentHeight() int {
	if m.height > 0 {
		return m.height
	}
	return 24
}

func measureTerminal() (int, int, bool) {
	// Bubble Tea sends WindowSizeMsg immediately. This fallback only improves the
	// first frame when a terminal implementation delays that message.
	return 0, 0, false
}

func tuiForeground(color string) lipgloss.Style {
	style := lipgloss.NewStyle()
	if os.Getenv("NO_COLOR") == "" {
		style = style.Foreground(lipgloss.Color(color))
	}
	return style
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type inventoryMsg struct {
	id        uint64
	inventory tuiInventory
	err       error
}

type sessionCheckMsg struct {
	id       uint64
	request  tuiActionRequest
	sessions []docker.ExecSession
	err      error
}

type interactiveDoneMsg struct {
	id      uint64
	request tuiActionRequest
	output  string
	err     error
}

type operationProgressMsg struct {
	id   uint64
	text string
}

type operationProgressWriter struct {
	ctx    context.Context
	id     uint64
	events chan<- tea.Msg
	mu     sync.Mutex
	buffer []byte
}

func (w *operationProgressWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	w.buffer = append(w.buffer, data...)
	if len(w.buffer) < 4096 {
		w.mu.Unlock()
		return len(data), nil
	}
	chunk := string(w.buffer)
	w.buffer = w.buffer[:0]
	w.mu.Unlock()
	if err := w.send(chunk); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (w *operationProgressWriter) Flush() error {
	w.mu.Lock()
	if len(w.buffer) == 0 {
		w.mu.Unlock()
		return nil
	}
	chunk := string(w.buffer)
	w.buffer = w.buffer[:0]
	w.mu.Unlock()
	return w.send(chunk)
}

func (w *operationProgressWriter) send(text string) error {
	select {
	case w.events <- operationProgressMsg{id: w.id, text: text}:
		return nil
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
}

type operationDoneMsg struct {
	id      uint64
	request tuiActionRequest
	err     error
}

type toastExpiredMsg struct{ id uint64 }
type refreshTickMsg struct{}

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Manage development containers interactively",
	RunE: func(cmd *cobra.Command, args []string) error {
		m := initialModel(cmd)
		program := tea.NewProgram(m, tea.WithContext(cmd.Context()))
		finalModel, err := program.Run()
		if err != nil {
			return err
		}
		final, ok := finalModel.(model)
		if !ok || final.pendingEnter == "" {
			return nil
		}
		// The interactive shell starts only after Bubble Tea restores the terminal.
		return runEnter(cmd, final.pendingEnter, false)
	},
}
