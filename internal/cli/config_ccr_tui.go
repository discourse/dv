package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"dv/internal/docker"
)

// Preset selection TUI

type presetItem struct {
	preset routerPreset
}

func (i presetItem) Title() string       { return i.preset.name }
func (i presetItem) Description() string { return i.preset.description }
func (i presetItem) FilterValue() string { return i.preset.name }

type presetModel struct {
	list     list.Model
	choice   *routerPreset
	quitting bool
}

func (m presetModel) Init() tea.Cmd {
	return nil
}

func (m presetModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "enter":
			if i, ok := m.list.SelectedItem().(presetItem); ok {
				m.choice = &i.preset
			}
			m.quitting = true
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.list.SetSize(msg.Width, msg.Height)
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m presetModel) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	return tea.NewView("\n" + m.list.View())
}

func selectRouterPreset() (*routerPreset, error) {
	items := make([]list.Item, len(routerPresets))
	for i, preset := range routerPresets {
		items[i] = presetItem{preset: preset}
	}

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("86")). // Cyan
		Padding(0, 1)

	delegate := list.NewDefaultDelegate()
	l := list.New(items, delegate, 0, 0)
	l.Title = "Select Router Preset"
	l.Styles.Title = titleStyle
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)

	m := presetModel{list: l}
	p := tea.NewProgram(m)

	final, err := p.Run()
	if err != nil {
		return nil, err
	}

	if finalModel, ok := final.(presetModel); ok && finalModel.choice != nil {
		return finalModel.choice, nil
	}

	return nil, fmt.Errorf("no preset selected")
}

// Custom router editor TUI

type routeEditorItem struct {
	route string
	model string
}

func (i routeEditorItem) Title() string       { return i.route }
func (i routeEditorItem) Description() string { return i.model }
func (i routeEditorItem) FilterValue() string { return i.route }

type routeEditorModel struct {
	list     list.Model
	routes   []string
	models   []string
	current  map[string]string
	quitting bool
}

func (m routeEditorModel) Init() tea.Cmd {
	return nil
}

func (m routeEditorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc":
			m.quitting = true
			return m, tea.Quit
		case "enter":
			// Edit the selected route
			idx := m.list.Index()
			if idx >= 0 && idx < len(m.routes) {
				route := m.routes[idx]
				currentModel := m.current[route]

				// Extract just the model name from "provider,model"
				parts := strings.Split(currentModel, ",")
				if len(parts) == 2 {
					currentModel = parts[1]
				}

				// Show model selector
				newModel, err := selectModelForRoute(m.models, currentModel)
				if err == nil && newModel != "" {
					m.current[route] = "openrouter," + newModel
					// Rebuild list
					items := make([]list.Item, len(m.routes))
					for i, r := range m.routes {
						modelName := m.current[r]
						parts := strings.Split(modelName, ",")
						if len(parts) == 2 {
							modelName = parts[1]
						}
						items[i] = routeEditorItem{route: r, model: modelName}
					}
					m.list.SetItems(items)
				}
			}
		case "ctrl+s":
			// Save and quit
			m.quitting = true
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.list.SetSize(msg.Width, msg.Height)
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m routeEditorModel) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	return tea.NewView("\n" + m.list.View() + "\n\nPress Enter to edit, Ctrl+S to save, Esc to cancel")
}

func selectCustomRouter(allModels []string, existingRouter map[string]string) (map[string]string, error) {
	routes := []string{"default", "background", "think", "longContext", "webSearch", "lowLatency"}

	// Initialize with existing or defaults
	current := make(map[string]string)
	for _, route := range routes {
		if existing, ok := existingRouter[route]; ok {
			current[route] = existing
		} else {
			// Use first model as default
			if len(allModels) > 0 {
				current[route] = "openrouter," + allModels[0]
			}
		}
	}

	items := make([]list.Item, len(routes))
	for i, route := range routes {
		modelName := current[route]
		parts := strings.Split(modelName, ",")
		if len(parts) == 2 {
			modelName = parts[1]
		}
		items[i] = routeEditorItem{route: route, model: modelName}
	}

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("86")).
		Padding(0, 1)

	delegate := list.NewDefaultDelegate()
	l := list.New(items, delegate, 0, 0)
	l.Title = "Configure Router (Enter to edit route)"
	l.Styles.Title = titleStyle
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)

	m := routeEditorModel{
		list:    l,
		routes:  routes,
		models:  allModels,
		current: current,
	}
	p := tea.NewProgram(m)

	final, err := p.Run()
	if err != nil {
		return nil, err
	}

	if finalModel, ok := final.(routeEditorModel); ok && !finalModel.quitting {
		return nil, fmt.Errorf("cancelled")
	} else if ok {
		return finalModel.current, nil
	}

	return nil, fmt.Errorf("selection failed")
}

// Model selector TUI

type modelSelectorItem struct {
	model string
}

func (i modelSelectorItem) Title() string       { return i.model }
func (i modelSelectorItem) Description() string { return "" }
func (i modelSelectorItem) FilterValue() string { return i.model }

type modelSelectorModel struct {
	list     list.Model
	choice   string
	quitting bool
}

func (m modelSelectorModel) Init() tea.Cmd {
	return nil
}

func (m modelSelectorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc":
			m.quitting = true
			return m, tea.Quit
		case "enter":
			if i, ok := m.list.SelectedItem().(modelSelectorItem); ok {
				m.choice = i.model
			}
			m.quitting = true
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.list.SetSize(msg.Width, msg.Height)
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m modelSelectorModel) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	return tea.NewView("\n" + m.list.View())
}

func selectModelForRoute(models []string, currentModel string) (string, error) {
	items := make([]list.Item, len(models))
	selectedIdx := 0
	for i, model := range models {
		items[i] = modelSelectorItem{model: model}
		if model == currentModel {
			selectedIdx = i
		}
	}

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("86")).
		Padding(0, 1)

	delegate := list.NewDefaultDelegate()
	l := list.New(items, delegate, 0, 0)
	l.Title = "Select Model"
	l.Styles.Title = titleStyle
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)
	l.Select(selectedIdx)

	m := modelSelectorModel{list: l}
	p := tea.NewProgram(m)

	final, err := p.Run()
	if err != nil {
		return "", err
	}

	if finalModel, ok := final.(modelSelectorModel); ok && finalModel.choice != "" {
		return finalModel.choice, nil
	}

	return "", fmt.Errorf("no model selected")
}

// Helper to read existing router config from container

func readExistingRouter(containerName, workdir, containerConfigPath string) (map[string]string, error) {
	out, err := docker.ExecOutput(containerName, workdir, nil, []string{"bash", "-lc", "cat " + shellQuote(containerConfigPath) + " 2>/dev/null"})
	if err != nil {
		return nil, err
	}

	var config map[string]interface{}
	if err := json.Unmarshal([]byte(out), &config); err != nil {
		return nil, err
	}

	router := make(map[string]string)
	if routerData, ok := config["Router"].(map[string]interface{}); ok {
		for k, v := range routerData {
			if str, ok := v.(string); ok {
				router[k] = str
			}
		}
	}

	return router, nil
}
