package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	textarea "github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/paste"
	"dv/internal/xdg"
)

// runAgentCmd implements `dv run-agent` (alias: `ra`).
// Usage:
//
//	dv ra <agent> [prompt_file|prompt words...]
//	dv ra <agent> -- [raw agent args...]
//
// If no prompt/args provided, an editor is opened to enter a multiline prompt.
// Prompt files are read from ~/.config/dv/prompts/ and autocompleted.
var runAgentCmd = &cobra.Command{
	Use:     "run-agent [--name NAME] AGENT [PROMPT_FILE|-- ARGS...|PROMPT ...]",
	Aliases: []string{"ra"},
	Short:   "Run an AI agent inside the container with a prompt or prompt file",
	Args:    cobra.MinimumNArgs(1),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// First arg: agent name completion
		if len(args) == 0 {
			// Use precomputed alias map (already deduped)
			var out []string
			pref := strings.ToLower(strings.TrimSpace(toComplete))
			for name := range agentAliasMap {
				if pref == "" || strings.HasPrefix(name, pref) {
					out = append(out, name)
				}
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		}

		// Second arg: prompt file completion
		if len(args) == 1 {
			// If the user appears to be typing a filesystem path, defer to the shell's default
			// file completion so regular files can be selected as prompts.
			if strings.HasPrefix(toComplete, "./") || strings.HasPrefix(toComplete, "../") || strings.HasPrefix(toComplete, "/") || strings.Contains(toComplete, string(os.PathSeparator)) {
				return nil, cobra.ShellCompDirectiveDefault
			}

			configDir, err := xdg.ConfigDir()
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}

			promptsDir := filepath.Join(configDir, "prompts")
			entries, err := os.ReadDir(promptsDir)
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}

			var suggestions []string
			for _, entry := range entries {
				if !entry.IsDir() {
					suggestions = append(suggestions, entry.Name())
				}
			}

			// Filter by prefix
			var out []string
			pref := strings.ToLower(strings.TrimSpace(toComplete))
			for _, s := range suggestions {
				if pref == "" || strings.HasPrefix(strings.ToLower(s), pref) {
					out = append(out, s)
				}
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		}

		// No completion for additional args
		return nil, cobra.ShellCompDirectiveNoFileComp
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}

		name, _ := cmd.Flags().GetString("name")
		if name == "" {
			name = currentAgentName(cfg)
		}

		// Ensure container exists and is running (match behavior of `enter`)
		if !docker.Exists(name) {
			fmt.Fprintf(cmd.OutOrStdout(), "Container '%s' does not exist. Run 'dv start' first.\n", name)
			return nil
		}
		if !docker.Running(name) {
			fmt.Fprintf(cmd.OutOrStdout(), "Starting container '%s'...\n", name)
			if err := docker.Start(name); err != nil {
				return err
			}
		}

		// Resolve workdir from image associated with container
		imgName := cfg.ContainerImages[name]
		var imgCfg config.ImageConfig
		if imgName != "" {
			imgCfg = cfg.Images[imgName]
		} else {
			_, imgCfg, err = resolveImage(cfg, "")
			if err != nil {
				return err
			}
		}
		workdir := config.EffectiveWorkdir(cfg, imgCfg, name)

		// Parse args: first token is the agent name (resolve aliases, returns lowercase)
		agent := resolveAgentAlias(args[0])

		// Copy configured files (auth, etc.) into the container as in `enter`,
		// but scoped to the requested agent when configured.
		copyConfiguredFiles(cmd, cfg, name, workdir, agent)

		envs := buildAgentEnv(cfg, agent, cmd)

		rawArgs := []string{}
		rest := args[1:]

		// If user provided "--" treat everything after as raw agent args (no prompt wrapping).
		// Cobra strips the literal "--" from args, so rely on ArgsLenAtDash to find the split.
		if dash := cmd.ArgsLenAtDash(); dash >= 0 {
			if dash < len(args) {
				rawArgs = append(rawArgs, args[dash:]...)
			}
			if dash > 1 {
				rest = args[1:dash]
			} else {
				rest = []string{}
			}
		} else if len(rest) > 0 {
			// Fallback: honor an explicit "--" if flag parsing was disabled in the future.
			for i, a := range rest {
				if a == "--" {
					rawArgs = append(rawArgs, rest[i+1:]...)
					rest = rest[:i]
					break
				}
			}
		}

		// Check if the first argument after agent is a prompt file
		var promptFromFile string
		if len(rest) > 0 {
			firstArg := rest[0]
			// 1) Prefer an actual host filesystem path if it exists (supports relative/absolute)
			hostPath := expandHostPath(firstArg)
			if st, err := os.Stat(hostPath); err == nil && st.Mode().IsRegular() {
				if content, err2 := os.ReadFile(hostPath); err2 == nil {
					promptFromFile = strings.TrimSpace(string(content))
					rest = rest[1:]
				}
			} else {
				// 2) Fallback to a named prompt under ~/.config/dv/prompts
				promptsDir := filepath.Join(configDir, "prompts")
				promptFilePath := filepath.Join(promptsDir, firstArg)
				if st2, err3 := os.Stat(promptFilePath); err3 == nil && st2.Mode().IsRegular() {
					if content, err4 := os.ReadFile(promptFilePath); err4 == nil {
						promptFromFile = strings.TrimSpace(string(content))
						rest = rest[1:]
					}
				}
			}
		}

		// Build the argv to run inside the container using internal rules.
		var argv []string
		switch {
		case len(rawArgs) > 0:
			argv = append([]string{agent}, rawArgs...)
			// If this is a pure help request, capture output via non-TTY exec
			if isHelpArgs(rawArgs) {
				shellCmd := withUserPaths(shellJoin(argv))
				out, err := docker.ExecOutput(name, workdir, envs, []string{"bash", "-lc", shellCmd})
				if err != nil {
					fmt.Fprint(cmd.ErrOrStderr(), out)
					return err
				}
				fmt.Fprint(cmd.OutOrStdout(), out)
				return nil
			}
		case promptFromFile != "":
			// Prompt from file -> construct one-shot invocation with implicit bypass flags
			argv = buildAgentArgs(agent, promptFromFile)
		case len(rest) == 0:
			// No prompt provided -> run interactively with implicit bypass flags
			argv = buildAgentInteractive(agent)
		default:
			// Prompt provided -> construct one-shot invocation with implicit bypass flags
			prompt := strings.Join(rest, " ")
			argv = buildAgentArgs(agent, prompt)
		}

		// Execute inside container through a login shell to pick up PATH/rc files
		shellCmd := withUserPaths(shellJoin(argv))

		// Check if paste support is enabled
		pasteEnabled, _ := cmd.Flags().GetBool("paste")
		if pasteEnabled {
			return paste.ExecWithPaste(paste.DockerExecConfig{
				ContainerName: name,
				Workdir:       workdir,
				Envs:          envs,
				Argv:          []string{"bash", "-lc", shellCmd},
				User:          "discourse",
			})
		}
		return docker.ExecInteractive(name, workdir, envs, []string{"bash", "-lc", shellCmd})
	},
}

// collectPromptInteractive opens $EDITOR for a multiline prompt; falls back to terminal input if needed.
func collectPromptInteractive(cmd *cobra.Command) (string, error) {
	// Use a small Bubble Tea textarea for multiline prompt collection
	m := newPromptModel()
	p := tea.NewProgram(m, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return "", err
	}
	pm, ok := final.(promptModel)
	if !ok {
		return "", fmt.Errorf("unexpected model type")
	}
	if pm.canceled {
		return "", nil
	}
	return strings.TrimSpace(pm.ta.Value()), nil
}

func buildAgentEnv(cfg config.Config, agent string, cmd *cobra.Command) docker.Envs {
	if agent == "ccr" {
		envs := make(docker.Envs, 0, 3)
		if _, ok := os.LookupEnv("TERM"); ok {
			envs = append(envs, "TERM")
		}
		if _, ok := os.LookupEnv("OPENROUTER_API_KEY"); ok {
			envs = append(envs, "OPENROUTER_API_KEY")
		} else {
			fmt.Fprintln(cmd.ErrOrStderr(), "Warning: OPENROUTER_API_KEY is not set on host; CCR may fail to authenticate.")
		}
		if _, ok := os.LookupEnv("OPENROUTER_KEY"); ok {
			envs = append(envs, "OPENROUTER_KEY")
		}
		return envs
	}

	envs := make(docker.Envs, 0, len(cfg.EnvPassthrough)+3)
	for _, key := range cfg.EnvPassthrough {
		if val, ok := os.LookupEnv(key); ok && strings.TrimSpace(val) != "" {
			// docker exec -e KEY will copy host value
			envs = append(envs, key)
		}
	}

	if rule, ok := agentRules[agent]; ok {
		envs = append(envs, rule.env...)
	}

	// Ensure a sane runtime environment for discourse user
	envs = append(envs,
		"HOME=/home/discourse",
		"USER=discourse",
		"SHELL=/bin/bash",
	)
	return envs
}

// buildAgentArgs uses internal, hard-coded rules per agent to construct argv.
// If the agent is unknown, falls back to positional prompt.
func buildAgentArgs(agent string, prompt string) []string {
	if rule, ok := agentRules[strings.ToLower(agent)]; ok {
		base := rule.withPrompt(prompt)
		if len(rule.defaults) > 0 {
			base = injectDefaults(base, rule.defaults)
		}
		return base
	}
	return []string{agent, prompt}
}

func buildAgentInteractive(agent string) []string {
	if rule, ok := agentRules[strings.ToLower(agent)]; ok {
		base := rule.interactive()
		if len(rule.defaults) > 0 {
			base = injectDefaults(base, rule.defaults)
		}
		return base
	}
	return []string{agent}
}

func injectDefaults(argv []string, defaults []string) []string {
	if len(argv) == 0 || len(defaults) == 0 {
		return argv
	}
	out := make([]string, 0, len(argv)+len(defaults))
	out = append(out, argv[0])
	out = append(out, defaults...)
	out = append(out, argv[1:]...)
	return out
}

// agentRule defines how to run each supported agent.
type agentRule struct {
	interactive func() []string
	withPrompt  func(prompt string) []string
	defaults    []string
	env         []string
	aliases     []string // alternative names for this agent
}

var agentRules = map[string]agentRule{
	"cursor": {
		interactive: func() []string { return []string{"cursor-agent"} },
		withPrompt:  func(p string) []string { return []string{"cursor-agent", "-p", p} },
		defaults:    []string{"-f"},
	},
	"ccr": {
		interactive: func() []string {
			return []string{"bash", "-c", "ccr stop 2>/dev/null || true; ccr code --dangerously-skip-permissions"}
		},
		withPrompt: func(p string) []string {
			return []string{"bash", "-c", "ccr stop 2>/dev/null || true; ccr code --dangerously-skip-permissions"}
		},
		defaults: []string{},
	},
	"codex": {
		interactive: func() []string { return []string{"codex"} },
		withPrompt:  func(p string) []string { return []string{"codex", "exec", "-s", "danger-full-access", p} },
		defaults:    []string{"--search", "--dangerously-bypass-approvals-and-sandbox", "--sandbox", "danger-full-access", "-c", "model_reasoning_effort=xhigh", "-m", "gpt-5.3-codex"},
	},
	"aider": {
		interactive: func() []string { return []string{"aider"} },
		withPrompt:  func(p string) []string { return []string{"aider", "--message", p} },
		defaults:    []string{"--yes-always"},
	},
	"claude": {
		interactive: func() []string { return []string{"claude"} },
		withPrompt:  func(p string) []string { return []string{"claude", "-p", p} },
		defaults:    []string{"--dangerously-skip-permissions"},
	},
	"gemini": {
		interactive: func() []string { return []string{"gemini"} },
		withPrompt:  func(p string) []string { return []string{"gemini", "-p", p} },
		defaults:    []string{"-y", "--include-directories", "/", "--model", "gemini-3-flash-preview"},
		env:         []string{"GEMINI_PROMPT_GIT=0"},
	},
	"crush": {
		interactive: func() []string { return []string{"crush"} },
		withPrompt:  func(p string) []string { return []string{"crush", "--prompt", p} },
		defaults:    []string{},
	},
	"amp": {
		interactive: func() []string { return []string{"amp"} },
		withPrompt:  func(p string) []string { return []string{"amp", "-x", p} },
		defaults:    []string{"--dangerously-allow-all"},
	},
	"opencode": {
		interactive: func() []string { return []string{"opencode"} },
		withPrompt:  func(p string) []string { return []string{"opencode", "run", p} },
		defaults:    []string{},
	},
	"copilot": {
		interactive: func() []string { return []string{"copilot"} },
		withPrompt:  func(p string) []string { return []string{"copilot", "-p", p} },
		defaults:    []string{"--allow-all-tools", "--allow-all-paths"},
	},
	"droid": {
		interactive: func() []string { return []string{"droid"} },
		withPrompt:  func(p string) []string { return []string{"droid", "exec", "--skip-permissions-unsafe", p} },
		defaults:    []string{},
	},
	"vibe": {
		interactive: func() []string { return []string{"vibe"} },
		withPrompt:  func(p string) []string { return []string{"vibe", "--prompt", p} },
		defaults:    []string{"--auto-approve"},
	},
	"term-llm": {
		interactive: func() []string { return []string{"term-llm"} },
		withPrompt:  func(p string) []string { return []string{"term-llm", "ask", "--yolo", p} },
		defaults:    []string{},
		aliases:     []string{"tl"},
		env:         []string{"GOOGLE_SEARCH_API_KEY", "GOOGLE_SEARCH_CX", "CEREBRAS_API_KEY"},
	},
}

// agentAliasMap maps aliases to canonical agent names (precomputed at init).
var agentAliasMap map[string]string

func init() {
	agentAliasMap = make(map[string]string)
	for canonical, rule := range agentRules {
		// Map canonical name to itself
		if existing, ok := agentAliasMap[canonical]; ok {
			panic("agent name collision: " + canonical + " already mapped to " + existing)
		}
		agentAliasMap[canonical] = canonical

		// Map aliases to canonical
		for _, alias := range rule.aliases {
			aliasLower := strings.ToLower(alias)
			if existing, ok := agentAliasMap[aliasLower]; ok {
				panic("agent alias collision: " + aliasLower + " already mapped to " + existing)
			}
			agentAliasMap[aliasLower] = canonical
		}
	}
}

// resolveAgentAlias returns the canonical agent name for a given alias (or the name itself).
func resolveAgentAlias(name string) string {
	lower := strings.ToLower(name)
	if canonical, ok := agentAliasMap[lower]; ok {
		return canonical
	}
	return lower
}

// shellJoin and shellQuote are now in shared.go

// withUserPaths prefixes a shell command with PATH extensions for common user-level bin dirs.
func withUserPaths(cmd string) string {
	prefix := "export PATH=\"$HOME/.local/bin:$HOME/bin:$HOME/.npm-global/bin:$HOME/.cargo/bin:$PATH\"; "
	return prefix + cmd
}

// ---- Minimal TUI for multiline prompt entry ----

type promptModel struct {
	ta       textarea.Model
	canceled bool
	width    int
	height   int
}

func newPromptModel() promptModel {
	ta := textarea.New()
	ta.Prompt = ""
	ta.Placeholder = "Type your prompt..."
	ta.ShowLineNumbers = false
	ta.Focus()
	w, h, ok := measureTerminal()
	if !ok || w <= 0 {
		w = 80
	}
	if !ok || h <= 0 {
		h = 24
	}
	pad := 6
	tw := w - pad
	if tw < 20 {
		tw = 20
	}
	th := h - pad
	if th > 20 {
		th = 20
	}
	ta.SetWidth(tw)
	ta.SetHeight(th)
	return promptModel{ta: ta, width: w, height: h}
}

func (m promptModel) Init() tea.Cmd { return nil }

func (m promptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {
	case tea.KeyMsg:
		switch t.Type {
		case tea.KeyEsc:
			m.canceled = true
			return m, tea.Quit
		case tea.KeyCtrlD, tea.KeyCtrlS:
			// Submit
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

func (m promptModel) View() string {
	hint := "Ctrl+D to run ? Esc to cancel"
	box := lipBox(m.ta.View()+"\n"+hint, m.width, m.height)
	return box
}

func lipBox(content string, termW, termH int) string {
	// Simple centered box without importing lipgloss here; reuse width sensibly.
	// Just return content; surrounding TUI already uses alt screen.
	return content
}

// isHelpArgs returns true when args are a simple help request like --help or -h
func isHelpArgs(args []string) bool {
	if len(args) == 1 {
		a := args[0]
		return a == "--help" || a == "-h" || a == "help"
	}
	return false
}

func init() {
	runAgentCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	runAgentCmd.Flags().Bool("paste", true, "Image paste support (copies pasted images to container); use --paste=false to disable")
}
