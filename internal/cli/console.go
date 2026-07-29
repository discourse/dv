package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/docker"
)

var consoleCmd = &cobra.Command{
	Use:   "console [NAME]",
	Short: "Open a Rails console in the container",
	Long: "Open an interactive Rails console in a Discourse container.\n\n" +
		"The command starts a stopped container, waits for PostgreSQL, and runs from the image's canonical Discourse workdir rather than a custom workspace override.",
	Args: cobra.MaximumNArgs(1),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return completeAgentNames(cmd, toComplete)
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		containerName := ""
		if len(args) == 1 {
			containerName = args[0]
		}

		target, err := resolveContainerTarget(cmd, containerName)
		if err != nil {
			return err
		}
		workdir, err := target.discourseWorkdir()
		if err != nil {
			return fmt.Errorf("dv console: %w", err)
		}
		if !docker.Exists(target.name) {
			return fmt.Errorf("container %q does not exist; run 'dv start' first", target.name)
		}
		if !docker.Running(target.name) {
			fmt.Fprintf(cmd.OutOrStdout(), "Starting container '%s'...\n", target.name)
			if err := startContainerWithPostStartHook(cmd, target.cfg, target.configDir, target.name, "console"); err != nil {
				return err
			}
		}

		// A Rails console needs API credentials from the host, but intentionally
		// does not apply copy rules: unlike enter/run, it is not a general agent
		// execution boundary and should not modify the worktree before opening.
		argv := []string{"bash", "-lc", buildConsoleScript(target.name)}
		if err := docker.ExecInteractive(target.name, workdir, collectEnvPassthrough(target.cfg), argv); err != nil {
			return fmt.Errorf("open Rails console in container %q: %w", target.name, err)
		}
		return nil
	},
}

func buildConsoleScript(containerName string) string {
	commands := []string{"set -e"}
	commands = append(commands, buildPostgresReadinessCommands()...)
	commands = append(commands,
		`pryrc=$(mktemp /tmp/dv-pryrc.XXXXXX)`,
		`trap 'rm -f "$pryrc"' EXIT`,
		`printf '%s\n' `+
			shellQuote(`xdg_config_home = ENV["XDG_CONFIG_HOME"]; xdg_config_home = File.expand_path("~/.config") if xdg_config_home.nil? || xdg_config_home.empty?`)+` `+
			shellQuote(`pryrc_candidates = [File.join(xdg_config_home, "pry", "pryrc"), File.expand_path("~/.pryrc")]`)+` `+
			shellQuote(`user_pryrc = pryrc_candidates.find { |candidate| File.file?(candidate) }`)+` `+
			shellQuote(`load user_pryrc if user_pryrc`)+` `+
			shellQuote("Pry.config.prompt_name = "+rubySingleQuoted("dv:"+containerName))+` > "$pryrc"`,
		`PRYRC="$pryrc" bin/rails console`,
	)
	return strings.Join(commands, "\n")
}

func rubySingleQuoted(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `'`, `\'`)
	return `'` + value + `'`
}

func init() {
	consoleCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
}
