package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

var stopCmd = &cobra.Command{
	Use:   "stop [name]",
	Short: "Stop the container or internal discourse services (pitchfork, ember-cli, etc.)",
	Args:  cobra.MaximumNArgs(1),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Complete container name for the first positional argument
		if len(args) == 0 {
			return completeAgentNames(cmd, toComplete)
		}
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

		// Priority: positional arg > --name flag > config
		name, _ := cmd.Flags().GetString("name")
		if len(args) > 0 {
			name = args[0]
		} else if name == "" {
			name = currentAgentName(cfg)
		}

		if !docker.Exists(name) {
			fmt.Fprintf(cmd.OutOrStdout(), "Container '%s' does not exist\n", name)
			return nil
		}
		if !docker.Running(name) {
			fmt.Fprintf(cmd.OutOrStdout(), "Container '%s' is already stopped\n", name)
			return nil
		}

		force, _ := cmd.Flags().GetBool("force")
		if proceed, err := warnActiveSessions(cmd, name, force); err != nil {
			return err
		} else if !proceed {
			return nil
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Stopping container '%s'...\n", name)
		return docker.Stop(name)
	},
}

func init() {
	stopCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	stopCmd.Flags().BoolP("force", "f", false, "Skip active session warning")
}
