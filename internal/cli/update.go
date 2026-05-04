package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/assets"
	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update tooling inside the container",
}

var updateAgentsCmd = &cobra.Command{
	Use:   "agents",
	Short: "Update all AI agents inside the container",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgentUpdates(cmd, "")
	},
}

var updateAgentCmd = &cobra.Command{
	Use:   "agent AGENT",
	Short: "Update one AI agent inside the container",
	Args:  cobra.ExactArgs(1),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeAgentUpdateNames(toComplete), cobra.ShellCompDirectiveNoFileComp
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgentUpdates(cmd, args[0])
	},
}

func runAgentUpdates(cmd *cobra.Command, agent string) error {
	steps, agentName, err := resolveAgentUpdateSteps(agent)
	if err != nil {
		return err
	}

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
	if name == "" {
		return fmt.Errorf("no agent selected; run 'dv start' to create one")
	}

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

	imgCfg, err := resolveImageConfig(cfg, name)
	if err != nil {
		return err
	}
	workdir := imgCfg.Workdir
	if workdir == "" {
		workdir = "/var/www/discourse"
	}

	if agentName == "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Updating AI agents in container '%s'...\n", name)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Updating %s in container '%s'...\n", steps[0].label, name)
	}

	for _, step := range steps {
		if err := runAgentUpdateStep(cmd, name, workdir, step); err != nil {
			return err
		}
	}

	if agentName == "" {
		fmt.Fprintln(cmd.OutOrStdout(), "All agents updated.")
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "%s updated.\n", steps[0].label)
	}
	return nil
}

var updateDiscourseCmd = &cobra.Command{
	Use:   "discourse",
	Short: "Update the Discourse image to latest using an embedded Dockerfile",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}

		// Determine which image to update (default: selected image)
		imageName, _ := cmd.Flags().GetString("image")
		if strings.TrimSpace(imageName) == "" {
			imageName = cfg.SelectedImage
		}
		imgCfg, ok := cfg.Images[imageName]
		if !ok {
			return fmt.Errorf("unknown image '%s'", imageName)
		}
		if imgCfg.Kind != "discourse" {
			return fmt.Errorf("'dv update discourse' only supports discourse-kind images; image '%s' is %q", imageName, imgCfg.Kind)
		}

		// Resolve the update Dockerfile to a local path
		dockerfilePath, contextDir, _, err := assets.ResolveDockerfileUpdateDiscourse(configDir)
		if err != nil {
			return err
		}

		// Build a temporary tag from the existing base image
		baseTag := imgCfg.Tag
		if strings.TrimSpace(baseTag) == "" {
			return fmt.Errorf("image '%s' has empty tag", imageName)
		}
		if !docker.ImageExists(baseTag) {
			return fmt.Errorf("base image tag '%s' does not exist; build it first with 'dv build'", baseTag)
		}

		// temp tag adds a suffix to avoid overwriting on failure
		tempTag := baseTag + ":updating"
		// If baseTag already contains a colon (repo:tag), preserve repo and use a separate temporary repo:tag if desired.
		// We will simply append -updated to the tag portion when colon is present.
		if strings.Contains(baseTag, ":") {
			// split on last colon
			idx := strings.LastIndex(baseTag, ":")
			repo := baseTag[:idx]
			tag := baseTag[idx+1:]
			tempTag = repo + ":" + tag + "-updating"
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Updating Discourse in image '%s' (tag %s) using %s...\n", imageName, baseTag, filepath.Base(dockerfilePath))

		// Build with BASE_IMAGE arg pointing at existing image tag
		buildArgs := []string{"--build-arg", "BASE_IMAGE=" + baseTag}
		opts := docker.BuildOptions{
			ExtraArgs: buildArgs,
		}
		if err := docker.BuildFrom(tempTag, dockerfilePath, contextDir, opts); err != nil {
			return err
		}

		// Retag tempTag to baseTag (overwrite baseTag to point at updated image)
		fmt.Fprintf(cmd.OutOrStdout(), "Retagging %s -> %s...\n", tempTag, baseTag)
		if err := docker.TagImage(tempTag, baseTag); err != nil {
			return err
		}

		fmt.Fprintln(cmd.OutOrStdout(), "Discourse image updated to latest.")
		return nil
	},
}

type agentUpdateStep struct {
	name         string
	aliases      []string
	label        string
	command      string
	runAsRoot    bool
	useUserPaths bool
}

var agentUpdateSteps = []agentUpdateStep{
	{name: "codex", label: "OpenAI Codex CLI", command: "npm install -g @openai/codex", runAsRoot: true},
	{name: "gemini", label: "Google Gemini CLI", command: "npm install -g @google/gemini-cli", runAsRoot: true},
	{name: "crush", label: "Crush CLI", command: "npm install -g @charmland/crush", runAsRoot: true},
	{name: "copilot", aliases: []string{"github"}, label: "Github CLI", command: "npm install -g @github/copilot", runAsRoot: true},
	{name: "opencode", label: "OpenCode AI", command: "npm install -g opencode-ai@latest", runAsRoot: true},
	{name: "amp", label: "Amp CLI", command: "npm install -g @sourcegraph/amp", runAsRoot: true},
	{name: "claude", label: "Claude CLI", command: "curl -fsSL https://claude.ai/install.sh | bash", useUserPaths: true},
	{name: "aider", label: "Aider", command: "curl -LsSf https://aider.chat/install.sh | sh", useUserPaths: true},
	{name: "cursor", aliases: []string{"cursor-agent"}, label: "Cursor Agent", command: "curl -fsS https://cursor.com/install | bash", useUserPaths: true},
	{name: "droid", aliases: []string{"factory", "factory-droid"}, label: "Factory Droid", command: "curl -fsSL https://app.factory.ai/cli | sh", useUserPaths: true},
	{name: "vibe", aliases: []string{"mistral", "mistral-vibe"}, label: "Mistral Vibe", command: "curl -LsSf https://mistral.ai/vibe/install.sh | bash", useUserPaths: true},
	{name: "term-llm", aliases: []string{"tl"}, label: "Term-LLM", command: "command -v term-llm >/dev/null && term-llm upgrade || echo 'term-llm not installed, skipping'", useUserPaths: true},
}

func resolveAgentUpdateSteps(agent string) ([]agentUpdateStep, string, error) {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" {
		return agentUpdateSteps, "", nil
	}

	for _, step := range agentUpdateSteps {
		if step.name == agent {
			return []agentUpdateStep{step}, step.name, nil
		}
		for _, alias := range step.aliases {
			if strings.ToLower(alias) == agent {
				return []agentUpdateStep{step}, step.name, nil
			}
		}
	}

	return nil, "", fmt.Errorf("unknown agent %q; expected one of: %s", agent, strings.Join(agentUpdateNames(), ", "))
}

func agentUpdateNames() []string {
	names := make([]string, 0, len(agentUpdateSteps))
	for _, step := range agentUpdateSteps {
		names = append(names, step.name)
	}
	return names
}

func completeAgentUpdateNames(toComplete string) []string {
	pref := strings.ToLower(strings.TrimSpace(toComplete))
	var out []string
	for _, step := range agentUpdateSteps {
		candidates := append([]string{step.name}, step.aliases...)
		for _, candidate := range candidates {
			candidate = strings.ToLower(candidate)
			if pref == "" || strings.HasPrefix(candidate, pref) {
				out = append(out, candidate)
			}
		}
	}
	return out
}

func runAgentUpdateStep(cmd *cobra.Command, containerName, workdir string, step agentUpdateStep) error {
	fmt.Fprintf(cmd.OutOrStdout(), "• %s...\n", step.label)

	shellCmd := "set -euo pipefail; "
	if step.useUserPaths {
		shellCmd += withUserPaths(step.command)
	} else {
		shellCmd += step.command
	}

	argv := []string{"bash", "-lc", shellCmd}
	var err error
	if step.runAsRoot {
		err = docker.ExecInteractiveAsRoot(containerName, workdir, nil, argv)
	} else {
		err = docker.ExecInteractive(containerName, workdir, nil, argv)
	}
	if err != nil {
		return fmt.Errorf("failed to update %s: %w", step.label, err)
	}
	return nil
}

func resolveImageConfig(cfg config.Config, containerName string) (config.ImageConfig, error) {
	if imgName, ok := cfg.ContainerImages[containerName]; ok {
		if imgCfg, found := cfg.Images[imgName]; found {
			return imgCfg, nil
		}
	}
	_, imgCfg, err := resolveImage(cfg, "")
	if err != nil {
		return config.ImageConfig{}, err
	}
	return imgCfg, nil
}

func init() {
	updateCmd.AddCommand(updateAgentsCmd)
	updateAgentsCmd.Flags().String("name", "", "Container name (defaults to selected or default)")

	updateCmd.AddCommand(updateAgentCmd)
	updateAgentCmd.Flags().String("name", "", "Container name (defaults to selected or default)")

	// dv update discourse
	updateCmd.AddCommand(updateDiscourseCmd)
	updateDiscourseCmd.Flags().String("image", "", "Image name to update (defaults to selected image)")
}
