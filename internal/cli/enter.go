package cli

import (
	"github.com/spf13/cobra"

	"dv/internal/docker"
)

var enterCmd = &cobra.Command{
	Use:   "enter [NAME]",
	Short: "Enter the container as 'discourse' (use --root for root)",
	Args:  cobra.MaximumNArgs(1),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Complete container name for the first positional argument
		if len(args) == 0 {
			return completeAgentNames(cmd, toComplete)
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		var containerName string
		if len(args) > 0 {
			containerName = args[0]
		}
		asRoot, _ := cmd.Flags().GetBool("root")
		return runEnter(cmd, containerName, asRoot)
	},
}

func runEnter(cmd *cobra.Command, containerName string, asRoot bool) error {
	ctx, ok, err := prepareContainerExecContext(cmd, containerName)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	execArgs := []string{"bash", "-l"}
	if asRoot {
		return docker.ExecInteractiveAsRoot(ctx.name, ctx.workdir, ctx.envs, execArgs)
	}
	return docker.ExecInteractive(ctx.name, ctx.workdir, ctx.envs, execArgs)
}

func init() {
	enterCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	enterCmd.Flags().Bool("root", false, "Enter as root user")
}
