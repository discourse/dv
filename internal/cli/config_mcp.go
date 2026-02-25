package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/discourse"
	"dv/internal/docker"
	"dv/internal/xdg"
)

var configMcpCmd = &cobra.Command{
	Use:   "mcp NAME",
	Short: "Configure an MCP server in the running container",
	Args:  cobra.ExactArgs(1),
	ValidArgs: []string{
		"playwright",
		"discourse",
		"chrome-devtools",
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

		containerName, _ := cmd.Flags().GetString("name")
		if containerName == "" {
			containerName = currentAgentName(cfg)
		}

		if !docker.Exists(containerName) {
			fmt.Fprintf(cmd.OutOrStdout(), "Container '%s' does not exist. Run 'dv start' first.\n", containerName)
			return nil
		}
		if !docker.Running(containerName) {
			fmt.Fprintf(cmd.OutOrStdout(), "Starting container '%s'...\n", containerName)
			if err := docker.Start(containerName); err != nil {
				return err
			}
		}

		// Determine workdir from the associated image if known; fall back to selected image
		imgName := cfg.ContainerImages[containerName]
		var imgCfg config.ImageConfig
		if imgName != "" {
			imgCfg = cfg.Images[imgName]
		} else {
			_, imgCfg, err = resolveImage(cfg, "")
			if err != nil {
				return err
			}
		}
		workdir := imgCfg.Workdir

		mcpName := strings.ToLower(strings.TrimSpace(args[0]))

		// Prepare env pass-through so tools like 'claude' have credentials
		envs := collectEnvPassthrough(cfg)
		if _, ok := os.LookupEnv("ANTHROPIC_API_KEY"); !ok {
			fmt.Fprintln(cmd.ErrOrStderr(), "Warning: ANTHROPIC_API_KEY is not set on host; 'claude' may fail.")
		}

		switch mcpName {
		case "playwright":
			return configurePlaywrightMCP(cmd, containerName, workdir, envs)
		case "discourse":
			return configureDiscourseMCP(cmd, containerName, workdir, envs)
		case "chrome-devtools":
			return configureChromeDevToolsMCP(cmd, containerName, workdir, envs)
		default:
			return fmt.Errorf("unsupported MCP name: %s (supported: playwright, discourse, chrome-devtools)", mcpName)
		}
	},
}

func init() {
	configMcpCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	configCmd.AddCommand(configMcpCmd)
}

// addOrReplaceTomlSection inserts or replaces a TOML table section defined by sectionHeader
// (e.g., "mcp_servers.playwright"). The sectionBody should include the full header line and
// any key/value lines below it, and may end with a trailing newline.
func addOrReplaceTomlSection(existing string, sectionHeader string, sectionBody string) string {
	// Normalize endings
	existing = strings.ReplaceAll(existing, "\r\n", "\n")
	lines := []string{}
	if strings.TrimSpace(existing) != "" {
		lines = strings.Split(existing, "\n")
	}
	headerLine := "[" + sectionHeader + "]"

	// Find existing section
	start := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == headerLine {
			start = i
			break
		}
	}

	if start == -1 {
		// Append section to the end
		var b strings.Builder
		if strings.TrimSpace(existing) != "" {
			b.WriteString(strings.TrimRight(existing, "\n"))
			b.WriteString("\n\n")
		}
		b.WriteString(strings.TrimRight(sectionBody, "\n"))
		b.WriteString("\n")
		return b.String()
	}

	// Determine end of existing section (next header or EOF)
	end := len(lines)
	for j := start + 1; j < len(lines); j++ {
		if strings.HasPrefix(strings.TrimSpace(lines[j]), "[") {
			end = j
			break
		}
	}
	// Rebuild with replacement
	var out []string
	out = append(out, lines[:start]...)
	// Add new body (without trailing newline to manage joins consistently)
	for _, l := range strings.Split(strings.TrimRight(sectionBody, "\n"), "\n") {
		out = append(out, l)
	}
	if end < len(lines) {
		// Ensure a blank line between sections if not already present
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "")
		}
		out = append(out, lines[end:]...)
	}
	// Ensure trailing newline
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}

func configurePlaywrightMCP(cmd *cobra.Command, containerName, workdir string, envs docker.Envs) error {
	mcpConfig := mcpConfiguration{
		name:            "playwright",
		registrationCmd: "claude mcp add -s user playwright -- npx -y @playwright/mcp@latest --isolated --no-sandbox --headless --executable-path /usr/bin/chromium",
		codexCommand:    "npx",
		codexArgs:       []string{"-y", "@playwright/mcp@latest", "--isolated", "--no-sandbox", "--headless", "--executable-path", "/usr/bin/chromium"},
		geminiCommand:   "npx",
		geminiArgs:      []string{"-y", "@playwright/mcp@latest", "--isolated", "--no-sandbox", "--headless", "--executable-path", "/usr/bin/chromium"},
	}
	return configureMCP(cmd, containerName, workdir, envs, mcpConfig)
}

func configureChromeDevToolsMCP(cmd *cobra.Command, containerName, workdir string, envs docker.Envs) error {
	mcpConfig := mcpConfiguration{
		name:            "chrome-devtools",
		registrationCmd: "claude mcp add -s user chrome-devtools -- npx -y chrome-devtools-mcp@latest --headless",
		codexCommand:    "npx",
		codexArgs:       []string{"-y", "chrome-devtools-mcp@latest", "--headless"},
		geminiCommand:   "npx",
		geminiArgs:      []string{"-y", "chrome-devtools-mcp@latest", "--headless"},
	}
	return configureMCP(cmd, containerName, workdir, envs, mcpConfig)
}

func configureDiscourseMCP(cmd *cobra.Command, containerName, workdir string, envs docker.Envs) error {
	// Dynamically determine the home directory for the discourse user
	homeDirRaw, err := docker.ExecOutput(containerName, "/", nil, []string{"bash", "-lc", "echo $HOME"})
	if err != nil {
		return fmt.Errorf("failed to determine home directory in container: %w", err)
	}
	homeDir := strings.TrimSpace(homeDirRaw)
	if homeDir == "" {
		homeDir = "/home/discourse" // fallback
	}

	profilePath := filepath.Join(homeDir, ".config/discourse-mcp/local.json")
	profileDir := filepath.Join(homeDir, ".config/discourse-mcp")
	const siteURL = "http://127.0.0.1:9292"
	const apiKeyDescription = "dv discourse-mcp"

	fmt.Fprintln(cmd.OutOrStdout(), "Preparing Discourse MCP profile with read/write access to local instance...")

	if _, err := docker.ExecOutput(containerName, workdir, nil, []string{"bash", "-lc", "mkdir -p " + profileDir}); err != nil {
		return fmt.Errorf("failed to ensure discourse-mcp config directory: %w", err)
	}

	readCmd := fmt.Sprintf("if [ -f %q ]; then cat %q; fi", profilePath, profilePath)
	existingProfile, err := docker.ExecOutput(containerName, workdir, nil, []string{"bash", "-lc", readCmd})
	if err != nil {
		return fmt.Errorf("failed to read existing MCP profile: %w", err)
	}

	profile := map[string]any{}
	if strings.TrimSpace(existingProfile) != "" {
		if err := json.Unmarshal([]byte(existingProfile), &profile); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: existing MCP profile is invalid JSON, regenerating it: %v\n", err)
			profile = map[string]any{}
		}
	}

	authPairs := extractAuthPairs(profile)
	var pair map[string]any
	for _, candidate := range authPairs {
		if site, ok := candidate["site"].(string); ok && strings.EqualFold(site, siteURL) {
			pair = candidate
			break
		}
	}
	if pair == nil {
		pair = map[string]any{}
		authPairs = append(authPairs, pair)
	}

	apiKey := ""
	adminUsername := ""
	if existing, ok := pair["api_key"].(string); ok {
		apiKey = strings.TrimSpace(existing)
	}
	if existingUsername, ok := pair["api_username"].(string); ok {
		adminUsername = strings.TrimSpace(existingUsername)
	}
	if apiKey == "" || adminUsername == "" {
		fmt.Fprintln(cmd.OutOrStdout(), "Generating Discourse API key for admin user inside container...")
		generated, err := discourse.GenerateAPIKey(discourse.GenerateAPIKeyOptions{
			ContainerName: containerName,
			Workdir:       workdir,
			Description:   apiKeyDescription,
			Envs:          envs,
			Verbose:       false,
		})
		if err != nil {
			return fmt.Errorf("failed to create Discourse API key: %w", err)
		}
		apiKey = generated.Key
		adminUsername = generated.Username
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "Reusing existing Discourse API key for admin user from MCP profile.")
	}

	pair["site"] = siteURL
	pair["api_key"] = apiKey
	pair["api_username"] = adminUsername
	profile["auth_pairs"] = authPairs
	profile["read_only"] = false
	profile["allow_writes"] = true
	profile["site"] = siteURL
	if _, ok := profile["log_level"]; !ok {
		profile["log_level"] = "info"
	}

	profileBytes, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal MCP profile: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "discourse-mcp-profile-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	tmpProfile := filepath.Join(tmpDir, "local.json")
	if err := os.WriteFile(tmpProfile, profileBytes, 0o600); err != nil {
		return fmt.Errorf("failed to write temporary MCP profile: %w", err)
	}

	if err := docker.CopyToContainerWithOwnership(containerName, tmpProfile, profilePath, false); err != nil {
		return fmt.Errorf("failed to copy MCP profile into container: %w", err)
	}

	mcpConfig := mcpConfiguration{
		name:            "discourse",
		registrationCmd: fmt.Sprintf("claude mcp add -s user discourse -- npx -y @discourse/mcp@latest --profile %s", profilePath),
		codexCommand:    "npx",
		codexArgs:       []string{"-y", "@discourse/mcp@latest", "--profile", profilePath},
		geminiCommand:   "npx",
		geminiArgs:      []string{"-y", "@discourse/mcp@latest", "--profile", profilePath},
	}

	return configureMCP(cmd, containerName, workdir, envs, mcpConfig)
}

// mcpConfiguration holds the configuration for setting up an MCP server
type mcpConfiguration struct {
	name            string            // MCP server name (e.g., "playwright", "discourse")
	registrationCmd string            // Command to register with Claude
	codexCommand    string            // Command for Codex config
	codexArgs       []string          // Arguments for Codex config
	geminiCommand   string            // Command for Gemini config
	geminiArgs      []string          // Arguments for Gemini config
	env             map[string]string // Environment variables for the MCP server
}

// configureMCP registers an MCP server with Claude, Codex, and Gemini
func configureMCP(cmd *cobra.Command, containerName, workdir string, envs docker.Envs, mcpConfig mcpConfiguration) error {
	// Dynamically determine the home directory for the discourse user
	homeDirRaw, err := docker.ExecOutput(containerName, "/", nil, []string{"bash", "-lc", "echo $HOME"})
	if err != nil {
		return fmt.Errorf("failed to determine home directory in container: %w", err)
	}
	homeDir := strings.TrimSpace(homeDirRaw)
	if homeDir == "" {
		homeDir = "/home/discourse" // fallback
	}

	codexConfigPath := filepath.Join(homeDir, ".codex/config.toml")
	geminiConfigPath := filepath.Join(homeDir, ".gemini/settings.json")

	// Remove existing Claude MCP entry
	removeEchoCmd := fmt.Sprintf("claude mcp remove -s user %s", mcpConfig.name)
	removeCmd := removeEchoCmd + " || true"

	fmt.Fprintf(cmd.OutOrStdout(), "Ensuring no existing Claude MCP '%s' remains (safe to ignore failures)...\n", mcpConfig.name)
	fmt.Fprintf(cmd.OutOrStdout(), "Running: %s\n\n", removeEchoCmd)
	if err := docker.ExecInteractive(containerName, workdir, envs, []string{"bash", "-lc", removeCmd}); err != nil {
		// Ignore errors from removal; proceed to add
	}

	// Register MCP with Claude
	fmt.Fprintf(cmd.OutOrStdout(), "Registering MCP '%s' with Claude inside container '%s'...\n", mcpConfig.name, containerName)
	fmt.Fprintf(cmd.OutOrStdout(), "Running: %s\n\n", mcpConfig.registrationCmd)

	if err := docker.ExecInteractive(containerName, workdir, envs, []string{"bash", "-lc", mcpConfig.registrationCmd}); err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp("", "mcp-config-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	// Configure Codex
	fmt.Fprintf(cmd.OutOrStdout(), "\nConfiguring Codex to use the %s MCP (updating ~/.codex/config.toml)...\n", mcpConfig.name)

	_, _ = docker.ExecOutput(containerName, workdir, nil, []string{"bash", "-lc", "mkdir -p ~/.codex"})
	existsOut, _ := docker.ExecOutput(containerName, workdir, nil, []string{"bash", "-lc", "test -f " + codexConfigPath + " && echo EXISTS || echo MISSING"})
	hasCodexConfig := strings.Contains(existsOut, "EXISTS")

	hostCodexCfg := filepath.Join(tmpDir, "codex-config.toml")
	var codexContent string
	if hasCodexConfig {
		if err := docker.CopyFromContainer(containerName, codexConfigPath, hostCodexCfg); err != nil {
			return fmt.Errorf("failed to copy existing Codex config: %w", err)
		}
		b, err := os.ReadFile(hostCodexCfg)
		if err != nil {
			return err
		}
		codexContent = string(b)
	}

	codexSectionHeader := fmt.Sprintf("mcp_servers.%s", mcpConfig.name)
	argsJSON, err := json.Marshal(mcpConfig.codexArgs)
	if err != nil {
		return fmt.Errorf("failed to marshal codex args: %w", err)
	}
	codexSectionBody := strings.Join([]string{
		"[" + codexSectionHeader + "]",
		fmt.Sprintf("command = %q", mcpConfig.codexCommand),
		fmt.Sprintf("args = %s", string(argsJSON)),
		"",
	}, "\n")

	codexUpdated := addOrReplaceTomlSection(codexContent, codexSectionHeader, codexSectionBody)
	if err := os.WriteFile(hostCodexCfg, []byte(codexUpdated), 0o644); err != nil {
		return err
	}

	if err := docker.CopyToContainerWithOwnership(containerName, hostCodexCfg, codexConfigPath, false); err != nil {
		return fmt.Errorf("failed to copy Codex config into container: %w", err)
	}

	// Configure Gemini
	fmt.Fprintf(cmd.OutOrStdout(), "\nConfiguring Gemini CLI to use the %s MCP (updating ~/.gemini/settings.json)...\n", mcpConfig.name)

	_, _ = docker.ExecOutput(containerName, workdir, nil, []string{"bash", "-lc", "mkdir -p ~/.gemini"})
	existsOut, _ = docker.ExecOutput(containerName, workdir, nil, []string{"bash", "-lc", "test -f " + geminiConfigPath + " && echo EXISTS || echo MISSING"})
	hasGeminiConfig := strings.Contains(existsOut, "EXISTS")

	hostGeminiCfg := filepath.Join(tmpDir, "gemini-settings.json")
	geminiSettings := map[string]any{}

	if hasGeminiConfig {
		if err := docker.CopyFromContainer(containerName, geminiConfigPath, hostGeminiCfg); err != nil {
			return fmt.Errorf("failed to copy existing Gemini settings: %w", err)
		}
		b, err := os.ReadFile(hostGeminiCfg)
		if err != nil {
			return err
		}
		if len(b) > 0 {
			if err := json.Unmarshal(b, &geminiSettings); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: existing Gemini settings are invalid JSON, regenerating: %v\n", err)
				geminiSettings = map[string]any{}
			}
		}
	}

	mcpServersRaw, ok := geminiSettings["mcpServers"]
	var mcpServers map[string]any
	if ok {
		mcpServers, _ = mcpServersRaw.(map[string]any)
	}
	if mcpServers == nil {
		mcpServers = map[string]any{}
	}

	serverCfg := map[string]any{
		"command": mcpConfig.geminiCommand,
		"args":    mcpConfig.geminiArgs,
	}
	if len(mcpConfig.env) > 0 {
		serverCfg["env"] = mcpConfig.env
	}
	mcpServers[mcpConfig.name] = serverCfg
	geminiSettings["mcpServers"] = mcpServers

	geminiUpdated, err := json.MarshalIndent(geminiSettings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(hostGeminiCfg, geminiUpdated, 0o644); err != nil {
		return err
	}

	if err := docker.CopyToContainerWithOwnership(containerName, hostGeminiCfg, geminiConfigPath, false); err != nil {
		return fmt.Errorf("failed to copy Gemini settings into container: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "%s MCP configuration complete.\n", strings.Title(mcpConfig.name))
	return nil
}

func extractAuthPairs(profile map[string]any) []map[string]any {
	raw, ok := profile["auth_pairs"]
	if !ok {
		return []map[string]any{}
	}
	arr, ok := raw.([]any)
	if !ok {
		return []map[string]any{}
	}
	pairs := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			pairs = append(pairs, m)
		}
	}
	return pairs
}
