package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	list "charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	viewport "charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"dv/internal/config"
	"dv/internal/xdg"
)

type agentItem struct {
	name   string
	image  string
	status string
	ports  []string
}

func (a agentItem) Title() string { return a.name }
func (a agentItem) Description() string {
	extra := ""
	if len(a.ports) > 0 {
		extra = "  " + strings.Join(a.ports, " ")
	}
	return fmt.Sprintf("%s  %s%s", a.image, a.status, extra)
}
func (a agentItem) FilterValue() string { return a.name }

type keyMap struct {
	New     key.Binding
	Start   key.Binding
	Stop    key.Binding
	Remove  key.Binding
	Rename  key.Binding
	Select  key.Binding
	Enter   key.Binding
	Extract key.Binding
	Build   key.Binding
	Images  key.Binding
	Refresh key.Binding
	Help    key.Binding
	Quit    key.Binding
}

func newKeyMap() keyMap {
	return keyMap{
		New:     key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new")),
		Start:   key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "start")),
		Stop:    key.NewBinding(key.WithKeys("k"), key.WithHelp("k", "stop")),
		Remove:  key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "remove")),
		Rename:  key.NewBinding(key.WithKeys("f2"), key.WithHelp("F2", "rename")),
		Select:  key.NewBinding(key.WithKeys("\n", "enter"), key.WithHelp("enter", "select")),
		Enter:   key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "enter shell")),
		Extract: key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "extract")),
		Build:   key.NewBinding(key.WithKeys("b"), key.WithHelp("b", "build")),
		Images:  key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "images...")),
		Refresh: key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "refresh")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

// ShortHelp implements help.KeyMap
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.New, k.Start, k.Stop, k.Remove, k.Rename, k.Select, k.Enter, k.Extract, k.Build, k.Images, k.Refresh, k.Help, k.Quit}
}

// FullHelp implements help.KeyMap
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.New, k.Start, k.Stop, k.Remove, k.Rename},
		{k.Select, k.Enter, k.Extract, k.Build},
		{k.Images, k.Refresh, k.Help, k.Quit},
	}
}

type model struct {
	list         list.Model
	keys         keyMap
	help         help.Model
	status       string
	cfg          config.Config
	configDir    string
	logVP        viewport.Model
	logText      string
	logHeight    int
	pendingEnter string
	width        int
	height       int
	showHelp     bool
	showRename   bool
	renameInput  textinput.Model
	renamingOld  string
}

func initialModel(cmd *cobra.Command) model {
	m := model{keys: newKeyMap(), help: help.New()}
	m.help.ShowAll = true
	m.list = list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	m.list.Title = "dv agents"
	m.list.SetShowHelp(false)
	m.list.SetShowStatusBar(false)
	m.list.SetFilteringEnabled(true)
	m.list.AdditionalShortHelpKeys = func() []key.Binding {
		return []key.Binding{m.keys.New, m.keys.Start, m.keys.Stop, m.keys.Remove, m.keys.Select, m.keys.Enter, m.keys.Extract, m.keys.Build, m.keys.Images, m.keys.Refresh, m.keys.Quit}
	}
	m.list.AdditionalFullHelpKeys = func() []key.Binding {
		return []key.Binding{m.keys.New, m.keys.Start, m.keys.Stop, m.keys.Remove, m.keys.Select, m.keys.Enter, m.keys.Extract, m.keys.Build, m.keys.Images, m.keys.Refresh, m.keys.Quit}
	}
	// Log viewport
	m.logHeight = 10
	m.logVP = viewport.New(viewport.WithWidth(0), viewport.WithHeight(m.logHeight))
	m.logVP.SetContent("")
	// Rename input
	m.renameInput = textinput.New()
	m.renameInput.Prompt = "New name: "
	m.renameInput.CharLimit = 100
	m.renameInput.Placeholder = "agent-new-name"
	// Fallback: set initial width/height from terminal if available
	if w, h, ok := measureTerminal(); ok {
		m.width, m.height = w, h
		m.help.SetWidth(w)
		m.logVP.SetWidth(w)
		m.logVP.SetHeight(m.logHeight)
		m.list.SetSize(w, h-(m.logHeight+4))
	}
	m.reload(cmd)
	return m
}

func (m *model) reload(cmd *cobra.Command) {
	configDir, err := xdg.ConfigDir()
	if err != nil {
		m.status = err.Error()
		return
	}
	m.configDir = configDir
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		m.status = err.Error()
		return
	}
	m.cfg = cfg
	items := m.fetchAgentItems(cfg)
	m.list.SetItems(items)
	m.status = fmt.Sprintf("image: %s | selected agent: %s", cfg.SelectedImage, currentAgentName(cfg))
}

func (m *model) fetchAgentItems(cfg config.Config) []list.Item {
	out, _ := runShell("docker ps -a --format '{{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}'")
	var items []list.Item
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 3 {
			continue
		}
		name, image, status := parts[0], parts[1], parts[2]
		if cfg.Images[cfg.SelectedImage].Tag != image {
			continue
		}
		var ports []string
		if len(parts) == 4 {
			ports = parseHostPortURLs(parts[3])
		}
		items = append(items, agentItem{name: name, image: image, status: status, ports: ports})
	}
	// Sort by name for stability
	sort.Slice(items, func(i, j int) bool { return items[i].(agentItem).name < items[j].(agentItem).name })
	return items
}

func (m model) Init() tea.Cmd { return nil }

func (m model) View() tea.View {
	view := tea.NewView(m.viewString())
	view.AltScreen = true
	return view
}

func (m model) viewString() string {
	var b strings.Builder
	if m.showHelp {
		// Modal content
		content := m.renderHelpModal()
		// Center with dimmed background
		return lipgloss.Place(m.currentWidth(), m.currentHeight(), lipgloss.Center, lipgloss.Center, content,
			lipgloss.WithWhitespaceChars("░"), lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("8"))))
	}
	if m.showRename {
		content := m.renderRenameModal()
		return lipgloss.Place(m.currentWidth(), m.currentHeight(), lipgloss.Center, lipgloss.Center, content,
			lipgloss.WithWhitespaceChars("░"), lipgloss.WithWhitespaceStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("8"))))
	}
	b.WriteString(m.list.View())
	b.WriteString("\n")
	if m.status != "" {
		b.WriteString(m.status)
		b.WriteString("\n")
	}
	b.WriteString("─ logs ─\n")
	b.WriteString(m.logVP.View())
	b.WriteString("\n")
	// Footer: key help
	b.WriteString(m.renderFooter())
	return b.String()
}

// renderFooter renders the condensed help footer.
func (m model) renderFooter() string {
	if m.width <= 0 {
		return m.help.View(m.keys)
	}
	// Build a single-line footer that fits the full width without wrapping
	order := []key.Binding{
		m.keys.New, m.keys.Start, m.keys.Stop, m.keys.Remove, m.keys.Rename,
		m.keys.Select, m.keys.Enter, m.keys.Extract, m.keys.Build,
		m.keys.Images, m.keys.Refresh, m.keys.Help, m.keys.Quit,
	}
	keyStyle := lipgloss.NewStyle().Faint(true)
	descStyle := lipgloss.NewStyle()
	sep := lipgloss.NewStyle().Faint(true).Render(" │ ")
	var parts []string
	for _, b := range order {
		h := b.Help()
		if h.Key == "" && h.Desc == "" {
			continue
		}
		parts = append(parts, keyStyle.Render(h.Key)+" "+descStyle.Render(h.Desc))
	}
	line := strings.Join(parts, sep)
	w := m.currentWidth()
	line = truncateToWidth(line, w)
	padding := w - lipgloss.Width(line)
	if padding > 0 {
		line = line + strings.Repeat(" ", padding)
	}
	bar := lipgloss.NewStyle().Faint(true).Render(line)
	return bar
}

func (m model) renderHelpModal() string {
	// Build a full help table grouped by rows
	rows := m.keys.FullHelp()
	// Style for modal box
	title := lipgloss.NewStyle().Bold(true).Underline(true).Render("dv shortcuts")
	kv := func(k key.Binding) string {
		return lipgloss.NewStyle().Faint(true).Render(k.Help().Key) + " " + k.Help().Desc
	}
	var sb strings.Builder
	sb.WriteString(title)
	sb.WriteString("\n\n")
	for _, row := range rows {
		parts := make([]string, 0, len(row))
		for _, k := range row {
			parts = append(parts, kv(k))
		}
		sb.WriteString(strings.Join(parts, "    "))
		sb.WriteString("\n")
	}
	box := lipgloss.NewStyle().Padding(1, 2).Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8")).Width(minInt(m.width-8, 100)).Render(sb.String())
	return box
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// truncateToWidth returns s truncated to at most w cells, appending … if truncated
func truncateToWidth(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	// Reserve 1 cell for ellipsis if possible
	target := w
	ellipsis := ""
	if w > 0 {
		target = w - 1
		ellipsis = "…"
	}
	b := strings.Builder{}
	width := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if width+rw > target {
			break
		}
		b.WriteRune(r)
		width += rw
	}
	return b.String() + ellipsis
}

// currentWidth/Height prefer last resize; fallback to runtime measure
func (m model) currentWidth() int {
	if m.width > 0 {
		return m.width
	}
	if w, _, ok := measureTerminal(); ok {
		return w
	}
	return lipgloss.Width(m.help.View(m.keys))
}

func (m model) currentHeight() int {
	if m.height > 0 {
		return m.height
	}
	if _, h, ok := measureTerminal(); ok {
		return h
	}
	return m.logHeight + 8
}

// measureTerminal tries stdout then stdin to detect current terminal size
func measureTerminal() (int, int, bool) {
	// Try stdout
	if intFd := int(os.Stdout.Fd()); term.IsTerminal(intFd) {
		if w, h, err := term.GetSize(intFd); err == nil && w > 0 && h > 0 {
			return w, h, true
		}
	}
	// Fallback stdin
	if intFd := int(os.Stdin.Fd()); term.IsTerminal(intFd) {
		if w, h, err := term.GetSize(intFd); err == nil && w > 0 && h > 0 {
			return w, h, true
		}
	}
	return 0, 0, false
}

func (m model) renderRenameModal() string {
	title := lipgloss.NewStyle().Bold(true).Underline(true).Render("Rename agent")
	hint := lipgloss.NewStyle().Faint(true).Render("Enter to confirm • Esc to cancel")
	input := m.renameInput.View()
	var sb strings.Builder
	sb.WriteString(title)
	sb.WriteString("\n\n")
	sb.WriteString("Old: ")
	sb.WriteString(m.renamingOld)
	sb.WriteString("\n")
	sb.WriteString(input)
	sb.WriteString("\n\n")
	sb.WriteString(hint)
	width := minInt(m.width-8, 80)
	return lipgloss.NewStyle().Padding(1, 2).Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8")).Width(width).Render(sb.String())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {
	case tea.WindowSizeMsg:
		logH := m.logHeight
		if logH < 5 {
			logH = 5
		}
		m.width, m.height = t.Width, t.Height
		m.help.SetWidth(t.Width)
		m.logVP.SetWidth(t.Width)
		m.logVP.SetHeight(logH)
		base := 4
		if m.status != "" {
			base++
		}
		m.list.SetSize(t.Width, t.Height-(logH+base))
	case tea.KeyPressMsg:
		// If rename modal is open, capture keys there
		if m.showRename {
			switch t.String() {
			case "enter":
				nameNew := strings.TrimSpace(m.renameInput.Value())
				if nameNew == "" || nameNew == m.renamingOld {
					m.showRename = false
					m.renameInput.Blur()
					return m, nil
				}
				out, err := runDvCapture("rename", m.renamingOld, nameNew)
				m.appendLog(fmt.Sprintf("$ dv rename %s %s\n%s\n", m.renamingOld, nameNew, out))
				m.showRename = false
				m.renameInput.Blur()
				if err != nil {
					return m, nil
				}
				return m, func() tea.Msg { return reloadMsg{} }
			case "esc":
				m.showRename = false
				m.renameInput.Blur()
				return m, nil
			default:
				var cmd tea.Cmd
				m.renameInput, cmd = m.renameInput.Update(t)
				return m, cmd
			}
		}
		switch {
		case key.Matches(t, m.keys.Quit):
			return m, tea.Quit
		case key.Matches(t, m.keys.Help):
			m.showHelp = !m.showHelp
			return m, nil
		case key.Matches(t, m.keys.Rename):
			current := m.selectedAgent()
			if current != "" {
				m.showRename = true
				m.renamingOld = current
				m.renameInput.SetValue(current)
				m.renameInput.CursorEnd()
				m.renameInput.Focus()
				return m, nil
			}
		case key.Matches(t, m.keys.Refresh):
			m.reload(nil)
		case key.Matches(t, m.keys.New):
			return m, m.cmdNew()
		case key.Matches(t, m.keys.Start):
			return m, m.cmdStart()
		case key.Matches(t, m.keys.Stop):
			return m, m.cmdStop()
		case key.Matches(t, m.keys.Remove):
			return m, m.cmdRemove()
		case key.Matches(t, m.keys.Select):
			return m, m.cmdSelect()
		case key.Matches(t, m.keys.Enter):
			return m, m.cmdEnter()
		case key.Matches(t, m.keys.Extract):
			return m, m.cmdExtract()
		case key.Matches(t, m.keys.Build):
			return m, m.cmdBuild()
		case key.Matches(t, m.keys.Images):
			return m, m.cmdCycleImage()
		}
	case logMsg:
		m.appendLog(string(t))
		return m, tea.Tick(10*time.Millisecond, func(time.Time) tea.Msg { return reloadMsg{} })
	case reloadMsg:
		m.reload(nil)
	case errMsg:
		m.appendLog(fmt.Sprintf("error: %v", t))
	case enterMsg:
		m.pendingEnter = string(t)
		return m, tea.Quit
	}

	var cmds []tea.Cmd
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	if cmd != nil {
		cmds = append(cmds, cmd)
	}
	v, vCmd := m.logVP.Update(msg)
	m.logVP = v
	if vCmd != nil {
		cmds = append(cmds, vCmd)
	}
	return m, tea.Batch(cmds...)
}

func (m model) selectedAgent() string {
	if it, ok := m.list.SelectedItem().(agentItem); ok {
		return it.name
	}
	return m.cfg.SelectedAgent
}

// Commands (async)
func (m model) cmdNew() tea.Cmd {
	return func() tea.Msg {
		name := autogenName()
		out, err := runDvCapture("new", name)
		if err != nil {
			// Return a log message; error will be visible in log output
			return logMsg(fmt.Sprintf("$ dv new %s\n%s\n", name, out))
		}
		time.Sleep(300 * time.Millisecond)
		// Return log then trigger reload via a separate command
		return logMsg(fmt.Sprintf("$ dv new %s\n%s\n", name, out))
	}
}

func (m model) cmdStart() tea.Cmd {
	return func() tea.Msg {
		agent := m.selectedAgent()
		if agent == "" {
			return errMsg(fmt.Errorf("no agent selected"))
		}
		out, err := runDvCapture("start", "--name", agent)
		m.appendLog(fmt.Sprintf("$ dv start --name %s\n%s\n", agent, out))
		if err != nil {
			return errMsg(err)
		}
		return reloadMsg{}
	}
}

func (m model) cmdStop() tea.Cmd {
	return func() tea.Msg {
		agent := m.selectedAgent()
		if agent == "" {
			return errMsg(fmt.Errorf("no agent selected"))
		}
		out, err := runDvCapture("stop", "--name", agent)
		m.appendLog(fmt.Sprintf("$ dv stop --name %s\n%s\n", agent, out))
		if err != nil {
			return errMsg(err)
		}
		return reloadMsg{}
	}
}

func (m model) cmdRemove() tea.Cmd {
	return func() tea.Msg {
		agent := m.selectedAgent()
		if agent == "" {
			return errMsg(fmt.Errorf("no agent selected"))
		}
		out, err := runDvCapture("remove", "--name", agent)
		m.appendLog(fmt.Sprintf("$ dv remove --name %s\n%s\n", agent, out))
		if err != nil {
			return errMsg(err)
		}
		return reloadMsg{}
	}
}

func (m model) cmdSelect() tea.Cmd {
	return func() tea.Msg {
		agent := m.selectedAgent()
		if agent == "" {
			return errMsg(fmt.Errorf("no agent selected"))
		}
		out, err := runDvCapture("select", agent)
		m.appendLog(fmt.Sprintf("$ dv select %s\n%s\n", agent, out))
		if err != nil {
			return errMsg(err)
		}
		return reloadMsg{}
	}
}

func (m model) cmdEnter() tea.Cmd {
	return func() tea.Msg {
		agent := m.selectedAgent()
		if agent == "" {
			return errMsg(fmt.Errorf("no agent selected"))
		}
		// Send enterMsg so Update handles quitting cleanly
		return enterMsg(agent)
	}
}

func (m model) cmdExtract() tea.Cmd {
	return func() tea.Msg {
		agent := m.selectedAgent()
		if agent == "" {
			return errMsg(fmt.Errorf("no agent selected"))
		}
		out, err := runDvCapture("extract", "--name", agent)
		m.appendLog(fmt.Sprintf("$ dv extract --name %s\n%s\n", agent, out))
		if err != nil {
			return errMsg(err)
		}
		return reloadMsg{}
	}
}

func (m model) cmdBuild() tea.Cmd {
	return func() tea.Msg {
		out, err := runDvCapture("build")
		m.appendLog(fmt.Sprintf("$ dv build\n%s\n", out))
		if err != nil {
			return errMsg(err)
		}
		return reloadMsg{}
	}
}

func (m model) cmdCycleImage() tea.Cmd {
	return func() tea.Msg {
		// Cycle through configured images
		names := make([]string, 0, len(m.cfg.Images))
		for n := range m.cfg.Images {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) == 0 {
			return reloadMsg{}
		}
		idx := sort.SearchStrings(names, m.cfg.SelectedImage)
		if idx >= len(names) || names[idx] != m.cfg.SelectedImage {
			idx = 0
		} else {
			idx = (idx + 1) % len(names)
		}
		out, err := runDvCapture("image", "select", names[idx])
		m.appendLog(fmt.Sprintf("$ dv image select %s\n%s\n", names[idx], out))
		if err != nil {
			return errMsg(err)
		}
		return reloadMsg{}
	}
}

type reloadMsg struct{}

type errMsg error
type logMsg string
type enterMsg string

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Open the interactive TUI",
	RunE: func(cmd *cobra.Command, args []string) error {
		m := initialModel(cmd)
		p := tea.NewProgram(m)
		finalModel, err := p.Run()
		if err != nil {
			return err
		}
		if mm, ok := finalModel.(model); ok {
			if mm.pendingEnter != "" {
				exe, _ := os.Executable()
				c := exec.Command(exe, "enter", "--name", mm.pendingEnter)
				c.Stdin = os.Stdin
				c.Stdout = os.Stdout
				c.Stderr = os.Stderr
				return c.Run()
			}
		}
		return nil
	},
}

// runCobra ensures common context on commands that rely on config/xgd.
func (m model) runCobra(run func(*cobra.Command) error) error {
	c := &cobra.Command{}
	// Provide IO wiring similar to real execution
	c.SetIn(os.Stdin)
	c.SetOut(os.Stdout)
	c.SetErr(os.Stderr)
	// Some commands read config via xdg helpers, which use envs; ensure they resolve
	ctx := context.Background()
	c.SetContext(ctx)
	return run(c)
}

// appendLog adds text to the log viewport and scrolls to bottom
func (m *model) appendLog(s string) {
	if strings.TrimSpace(s) == "" {
		return
	}
	if m.logText != "" && !strings.HasSuffix(m.logText, "\n") {
		m.logText += "\n"
	}
	m.logText += s
	m.logVP.SetContent(m.logText)
	m.logVP.GotoBottom()
}

// runDvCapture executes the current dv binary with args and returns combined output
func runDvCapture(args ...string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(exe, args...)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return "", err
	}
	outBytes, _ := io.ReadAll(stdout)
	errBytes, _ := io.ReadAll(stderr)
	waitErr := cmd.Wait()
	out := string(outBytes) + string(errBytes)
	if waitErr != nil {
		return out, waitErr
	}
	return out, nil
}
