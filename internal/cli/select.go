package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/session"
	"dv/internal/xdg"
)

var selectCmd = &cobra.Command{
	Use:   "select NAME",
	Short: "Select an existing (or future) agent by name",
	Args:  cobra.ExactArgs(1),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Complete NAME
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

		name := args[0]

		// Priority 1: session-local state (pid-based, ancestor-process matching)
		if err := session.SetCurrentAgent(name); err != nil {
			return fmt.Errorf("could not save session state: %w", err)
		}

		// Priority 2: global config (fallback for new terminals)
		if err := config.Update(configDir, func(cfg *config.Config) error {
			cfg.SelectedAgent = name
			return nil
		}); err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Selected agent: %s\n", name)
		requestShellAgent(name)
		return nil
	},
}
