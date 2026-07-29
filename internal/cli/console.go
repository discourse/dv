package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"dv/internal/docker"
)

var consoleCmd = &cobra.Command{
	Use:   "console [NAME]",
	Short: "Open a Rails console in the container",
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

		ctx, ok, err := prepareContainerExecContext(cmd, containerName)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}

		// Label the pry prompt with the container name (e.g. "[1] dv:agent(main)>")
		// so it's obvious the console is running inside a dv container. Pry reads
		// the rc file pointed at by PRYRC instead of ~/.pryrc.
		rcLine := fmt.Sprintf("Pry.config.prompt_name = %q", "dv:"+ctx.name)
		script := fmt.Sprintf("echo %s > /tmp/.dv-pryrc && PRYRC=/tmp/.dv-pryrc exec bin/rails console", shellQuote(rcLine))
		execArgs := []string{"bash", "-lc", script}
		return docker.ExecInteractive(ctx.name, ctx.workdir, ctx.envs, execArgs)
	},
}

func init() {
	consoleCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
}
