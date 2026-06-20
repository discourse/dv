package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/resources"
	"dv/internal/xdg"
)

type aiToolCommandContext struct {
	cfg           *config.Config
	configDir     string
	containerName string
	discourseRoot string
}

type aiToolPreset struct {
	PresetID    string            `json:"preset_id"`
	PresetName  string            `json:"preset_name"`
	Name        string            `json:"name"`
	ToolName    string            `json:"tool_name"`
	Description string            `json:"description"`
	Summary     string            `json:"summary"`
	Script      string            `json:"script"`
	Parameters  []aiToolParameter `json:"parameters"`
}

type aiToolParameter struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Required    bool     `json:"required"`
	Enum        []string `json:"enum"`
}

type aiToolSkeletonPayload struct {
	DisplayName       string
	ToolName          string
	Description       string
	Summary           string
	Script            string
	WorkspacePath     string
	ContainerName     string
	DiscourseRoot     string
	PresetName        string
	PresetDescription string
	Parameters        []aiToolParameter
}

var configAiToolCmd = &cobra.Command{
	Use:   "ai-tool [NAME]",
	Short: "Create an AI tool workspace and point the container workdir at it",
	Long: `Scaffolds a directory under /home/discourse/ai-tools inside the selected container that
contains tool.yml (metadata), script.js (sandboxed JavaScript), a test payload, helper scripts,
and an AGENTS.md playbook. Run './bin/test' to exercise the tool and './bin/sync' to upsert it
into Discourse once ready. The container workdir is updated so 'dv enter' opens the workspace.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}

		containerOverride, _ := cmd.Flags().GetString("container")
		containerName := strings.TrimSpace(containerOverride)
		if containerName == "" {
			containerName = currentAgentName(cfg)
		}
		if strings.TrimSpace(containerName) == "" {
			fmt.Fprintln(cmd.ErrOrStderr(), "No container selected. Run 'dv start' first.")
			return nil
		}

		if !docker.Exists(containerName) {
			fmt.Fprintf(cmd.OutOrStdout(), "Container '%s' does not exist. Run 'dv start' first.\n", containerName)
			return nil
		}
		if !docker.Running(containerName) {
			fmt.Fprintf(cmd.OutOrStdout(), "Starting container '%s'...\n", containerName)
			if err := startContainerWithPostStartHook(cmd, cfg, configDir, containerName, "config ai-tool"); err != nil {
				return err
			}
		}

		imgName := cfg.ContainerImages[containerName]
		var imgCfg config.ImageConfig
		if imgName != "" {
			imgCfg = cfg.Images[imgName]
		} else {
			if _, resolved, err := resolveImage(cfg, ""); err == nil {
				imgCfg = resolved
			} else {
				return err
			}
		}
		discourseRoot := strings.TrimSpace(imgCfg.Workdir)
		if discourseRoot == "" {
			discourseRoot = "/var/www/discourse"
		}

		ctx := aiToolCommandContext{
			cfg:           &cfg,
			configDir:     configDir,
			containerName: containerName,
			discourseRoot: discourseRoot,
		}

		if err := ensureAiToolsRoot(ctx.containerName); err != nil {
			return err
		}

		displayName := ""
		if len(args) > 0 {
			displayName = strings.TrimSpace(args[0])
		}
		if displayName == "" {
			var err error
			displayName, err = promptAiToolName(cmd)
			if err != nil {
				return err
			}
		}

		toolNameFlag, _ := cmd.Flags().GetString("tool-name")
		toolName := deriveToolFunctionName(displayName, toolNameFlag)

		slug := aiToolDirSlug(displayName)
		workspacePath := path.Join("/home/discourse/ai-tools", slug)
		if err := ensureContainerPathAvailable(ctx.containerName, workspacePath); err != nil {
			return err
		}

		presets, err := fetchAiToolPresets(ctx)
		if err != nil {
			return err
		}
		if len(presets) == 0 {
			return errors.New("no AI tool presets returned from Discourse")
		}

		presetFlag, _ := cmd.Flags().GetString("preset")
		selectedPreset, err := selectAiToolPreset(cmd, presets, presetFlag)
		if err != nil {
			return err
		}

		if selectedPreset.Parameters == nil {
			selectedPreset.Parameters = []aiToolParameter{}
		}

		payload := aiToolSkeletonPayload{
			DisplayName:       displayName,
			ToolName:          toolName,
			Description:       selectedPreset.Description,
			Summary:           selectedPreset.Summary,
			Script:            selectedPreset.Script,
			WorkspacePath:     workspacePath,
			ContainerName:     ctx.containerName,
			DiscourseRoot:     ctx.discourseRoot,
			PresetName:        selectedPreset.PresetName,
			PresetDescription: selectedPreset.Description,
			Parameters:        selectedPreset.Parameters,
		}

		if strings.TrimSpace(payload.Description) == "" {
			payload.Description = fmt.Sprintf("%s custom tool", displayName)
		}
		if strings.TrimSpace(payload.Summary) == "" {
			payload.Summary = fmt.Sprintf("Workflow for %s", displayName)
		}

		tempDir, err := os.MkdirTemp("", "dv-ai-tool-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tempDir)
		root := filepath.Join(tempDir, "tool")
		if err := writeAiToolSkeleton(root, payload); err != nil {
			return err
		}

		if err := docker.CopyToContainerWithOwnership(ctx.containerName, root, workspacePath, true); err != nil {
			return err
		}

		if err := setContainerWorkdir(ctx.cfg, ctx.configDir, ctx.containerName, workspacePath); err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "AI tool workspace '%s' ready at %s (preset: %s).\n", displayName, workspacePath, selectedPreset.PresetName)
		fmt.Fprintf(cmd.OutOrStdout(), "Use 'dv enter' then run './bin/test' to iterate and './bin/sync' to upsert the tool.\n")
		return nil
	},
}

func init() {
	configAiToolCmd.Flags().String("container", "", "Container to configure (defaults to the selected agent)")
	configAiToolCmd.Flags().String("preset", "", "Preset ID to seed the workspace (default: empty_tool)")
	configAiToolCmd.Flags().String("tool-name", "", "Override the generated tool_name (must be alphanumeric + underscores)")
	configCmd.AddCommand(configAiToolCmd)
}

func ensureAiToolsRoot(containerName string) error {
	cmd := "mkdir -p /home/discourse/ai-tools"
	if _, err := docker.ExecOutput(containerName, "/home/discourse", nil, []string{"bash", "-lc", cmd}); err != nil {
		return fmt.Errorf("failed to create /home/discourse/ai-tools: %w", err)
	}
	return nil
}

func fetchAiToolPresets(ctx aiToolCommandContext) ([]aiToolPreset, error) {
	ruby := `
require "json"
payload = AiTool.presets.map do |preset|
  {
    preset_id: preset[:preset_id],
    preset_name: preset[:preset_name],
    name: preset[:name],
    tool_name: preset[:tool_name],
    description: preset[:description],
    summary: preset[:summary],
    script: preset[:script],
    parameters: preset[:parameters],
  }
end
STDOUT.sync = true
print(JSON.generate(payload))
`
	script := fmt.Sprintf("cd %s && bundle exec rails runner %s", shellQuote(ctx.discourseRoot), shellQuote(ruby))
	out, err := docker.ExecOutput(ctx.containerName, ctx.discourseRoot, nil, []string{"bash", "-lc", script})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch AI tool presets: %v\n%s", err, strings.TrimSpace(out))
	}
	var presets []aiToolPreset
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &presets); err != nil {
		return nil, fmt.Errorf("failed to parse preset JSON: %w", err)
	}
	return presets, nil
}

func selectAiToolPreset(cmd *cobra.Command, presets []aiToolPreset, presetID string) (aiToolPreset, error) {
	if presetID != "" {
		for _, preset := range presets {
			if preset.PresetID == presetID {
				return preset, nil
			}
		}
		return aiToolPreset{}, fmt.Errorf("unknown preset %q", presetID)
	}

	defaultIndex := 0
	for i, preset := range presets {
		if preset.PresetID == "empty_tool" {
			defaultIndex = i
			break
		}
	}

	if !isTerminalInput() {
		return presets[defaultIndex], nil
	}

	fmt.Fprintln(cmd.OutOrStdout(), "Available presets:")
	for idx, preset := range presets {
		fmt.Fprintf(cmd.OutOrStdout(), "  [%d] %s — %s (%s)\n", idx+1, preset.PresetName, preset.Description, preset.PresetID)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Select preset [%d]: ", defaultIndex+1)

	reader := bufio.NewReader(cmd.InOrStdin())
	input, err := reader.ReadString('\n')
	if err != nil {
		return aiToolPreset{}, err
	}
	value := strings.TrimSpace(input)
	if value == "" {
		return presets[defaultIndex], nil
	}
	index, err := strconv.Atoi(value)
	if err != nil || index < 1 || index > len(presets) {
		return aiToolPreset{}, fmt.Errorf("invalid selection %q", value)
	}
	return presets[index-1], nil
}

func promptAiToolName(cmd *cobra.Command) (string, error) {
	if !isTerminalInput() {
		return "", errors.New("stdin is not interactive; pass a name argument")
	}
	fmt.Fprint(cmd.OutOrStdout(), "AI tool name: ")
	reader := bufio.NewReader(cmd.InOrStdin())
	value, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", errors.New("tool name cannot be empty")
	}
	return trimmed, nil
}

func deriveToolFunctionName(displayName, override string) string {
	if trimmed := strings.TrimSpace(override); trimmed != "" {
		return sanitizeToolName(trimmed)
	}
	base := sanitizeToolName(displayName)
	if base == "" {
		return "custom_tool"
	}
	return base
}

func sanitizeToolName(input string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range input {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			lastUnderscore = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(unicode.ToLower(r))
			lastUnderscore = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		case r == '_', r == '-', unicode.IsSpace(r):
			if b.Len() == 0 || lastUnderscore {
				continue
			}
			b.WriteRune('_')
			lastUnderscore = true
		default:
			// skip other characters
		}
	}
	result := strings.Trim(b.String(), "_")
	if result == "" {
		return ""
	}
	if len(result) > 100 {
		result = result[:100]
	}
	return result
}

func aiToolDirSlug(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return "ai-tool-workspace"
	}
	var builder strings.Builder
	lastDash := false
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_':
			if !lastDash {
				builder.WriteRune('-')
				lastDash = true
			}
		case unicode.IsSpace(r):
			if !lastDash {
				builder.WriteRune('-')
				lastDash = true
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if !lastDash {
				builder.WriteRune('-')
				lastDash = true
			}
		default:
			if !lastDash {
				builder.WriteRune('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if slug == "" {
		slug = "workspace"
	}
	return fmt.Sprintf("ai-tool-%s", slug)
}

func writeAiToolSkeleton(root string, payload aiToolSkeletonPayload) error {
	dirs := []string{
		"bin",
		"scripts",
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return err
		}
	}

	configContent, err := resources.RenderAiToolConfig(resources.AiToolConfigTemplateData{
		DisplayName:           payload.DisplayName,
		Name:                  payload.DisplayName,
		ToolName:              payload.ToolName,
		Summary:               payload.Summary,
		Description:           payload.Description,
		Parameters:            convertParametersForTemplate(payload.Parameters),
		RagChunkTokens:        374,
		RagChunkOverlapTokens: 10,
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "tool.yml"), []byte(configContent), 0o644); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(root, "script.js"), []byte(payload.Script), 0o644); err != nil {
		return err
	}

	testPayload := buildTestPayload(payload.Parameters)
	testBytes, err := json.MarshalIndent(testPayload, "", "  ")
	if err != nil {
		return err
	}
	testBytes = append(testBytes, '\n')
	if err := os.WriteFile(filepath.Join(root, "test_payload.json"), testBytes, 0o644); err != nil {
		return err
	}

	readme := fmt.Sprintf("# %s AI Tool\n\nBootstrapped via `dv config ai-tool`.\n\n- Run `./bin/test` to execute the tool using test_payload.json.\n- Run `./bin/sync` to upsert it into Discourse.\n", payload.DisplayName)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(readme), 0o644); err != nil {
		return err
	}

	agentContent, err := resources.RenderAiToolAgent(resources.AiToolAgentData{
		ToolDisplayName:   payload.DisplayName,
		ToolName:          payload.ToolName,
		WorkspacePath:     payload.WorkspacePath,
		ConfigPath:        path.Join(payload.WorkspacePath, "tool.yml"),
		ScriptPath:        path.Join(payload.WorkspacePath, "script.js"),
		TestPayloadPath:   path.Join(payload.WorkspacePath, "test_payload.json"),
		ContainerName:     payload.ContainerName,
		DiscourseRoot:     payload.DiscourseRoot,
		PresetName:        payload.PresetName,
		PresetDescription: payload.PresetDescription,
		ParameterSummary:  toParameterSummary(payload.Parameters),
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(agentContent), 0o644); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(root, "scripts", "sync_ai_tool.rb"), []byte(resources.AiToolSyncScript()), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "test_ai_tool.rb"), []byte(resources.AiToolTestScript()), 0o644); err != nil {
		return err
	}

	if err := writeBinScript(filepath.Join(root, "bin", "sync"), payload.DiscourseRoot, path.Join(payload.WorkspacePath, "scripts", "sync_ai_tool.rb"), false); err != nil {
		return err
	}
	if err := writeBinScript(filepath.Join(root, "bin", "test"), payload.DiscourseRoot, path.Join(payload.WorkspacePath, "scripts", "test_ai_tool.rb"), true); err != nil {
		return err
	}

	return nil
}

func convertParametersForTemplate(params []aiToolParameter) []resources.AiToolParameterTemplateData {
	out := make([]resources.AiToolParameterTemplateData, 0, len(params))
	for _, param := range params {
		out = append(out, resources.AiToolParameterTemplateData{
			Name:        param.Name,
			Type:        param.Type,
			Description: param.Description,
			Required:    param.Required,
			Enum:        append([]string(nil), param.Enum...),
		})
	}
	return out
}

func buildTestPayload(params []aiToolParameter) map[string]any {
	payload := make(map[string]any)
	for _, param := range params {
		payload[param.Name] = defaultTestValue(param)
	}
	return payload
}

func defaultTestValue(param aiToolParameter) any {
	switch strings.ToLower(strings.TrimSpace(param.Type)) {
	case "number":
		return 0
	case "boolean":
		return false
	case "array":
		return []any{}
	}
	if len(param.Enum) > 0 {
		return param.Enum[0]
	}
	return fmt.Sprintf("<%s>", param.Name)
}

func toParameterSummary(params []aiToolParameter) []resources.AiToolParameterSummary {
	out := make([]resources.AiToolParameterSummary, 0, len(params))
	for _, param := range params {
		out = append(out, resources.AiToolParameterSummary{
			Name:        param.Name,
			Type:        param.Type,
			Required:    param.Required,
			Description: param.Description,
		})
	}
	return out
}

func writeBinScript(dst, discourseRoot, runnerPath string, forwardArgs bool) error {
	content := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export DV_AI_TOOL_ROOT="$ROOT"
cd %s
bundle exec rails runner %s`, shellQuote(discourseRoot), shellQuote(runnerPath))
	if forwardArgs {
		content += ` "$@"`
	}
	content += "\n"
	return os.WriteFile(dst, []byte(content), 0o755)
}
