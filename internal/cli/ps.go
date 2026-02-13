package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

var psCmd = &cobra.Command{
	Use:   "ps [name]",
	Short: "Show active exec sessions in a container",
	Args:  cobra.MaximumNArgs(1),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
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
			fmt.Fprintf(cmd.OutOrStdout(), "Container '%s' is not running\n", name)
			return nil
		}

		sessions, err := docker.ExecSessions(name)
		if err != nil {
			return err
		}

		if len(sessions) == 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "No active sessions in '%s'\n", name)
			return nil
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Active sessions in '%s':\n", name)
		for _, s := range sessions {
			label := classifySession(s.Command)
			cmdDisplay := truncateCmd(s.Command, 40)
			fmt.Fprintf(cmd.OutOrStdout(), "  PID %-8d %-12s %-42s (%s)\n", s.PID, s.User, cmdDisplay, label)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "\n%d active session(s)\n", len(sessions))
		return nil
	},
}

func init() {
	psCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
}
