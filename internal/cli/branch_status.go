package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/docker"
)

// branchStatusCmd implements: dv branch status [--name NAME]
// Shows which branch is checked out in the container's repo workdir and
// whether the working tree has uncommitted changes.
var branchStatusCmd = &cobra.Command{
	Use:   "status [--name NAME]",
	Short: "Show the container's current branch and any uncommitted changes",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, ok, err := prepareContainerExecContext(cmd)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}

		branch, err := docker.ExecOutput(ctx.name, ctx.workdir, nil, []string{"git", "rev-parse", "--abbrev-ref", "HEAD"})
		if err != nil {
			return fmt.Errorf("container: failed to determine current branch: %w", err)
		}
		branch = strings.TrimSpace(branch)
		if branch == "HEAD" {
			sha, err := docker.ExecOutput(ctx.name, ctx.workdir, nil, []string{"git", "rev-parse", "--short", "HEAD"})
			if err == nil {
				branch = fmt.Sprintf("detached at %s", strings.TrimSpace(sha))
			}
		}

		changes, err := docker.ExecOutput(ctx.name, ctx.workdir, nil, []string{"git", "status", "--porcelain"})
		if err != nil {
			return fmt.Errorf("container: failed to check for changes: %w", err)
		}
		changes = strings.TrimRight(changes, "\n")

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Container: %s\n", ctx.name)
		fmt.Fprintf(out, "Branch:    %s\n", branch)
		if changes == "" {
			fmt.Fprintln(out, "Changes:   none (working tree clean)")
		} else {
			fmt.Fprintf(out, "Changes:\n%s\n", changes)
		}
		return nil
	},
}

func init() {
	branchStatusCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	branchCmd.AddCommand(branchStatusCmd)
}
