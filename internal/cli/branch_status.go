package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/docker"
)

type branchWorktreeStatus struct {
	branch  string
	changes string
}

type branchStatusExecFunc func(name, workdir string, envs docker.Envs, argv []string) (stdout, stderr string, err error)

type branchStatusDependencies struct {
	resolve func(cmd *cobra.Command, overrideName ...string) (resolvedContainerTarget, error)
	exists  func(name string) bool
	running func(name string) bool
	inspect func(containerName, workdir string, exec branchStatusExecFunc) (branchWorktreeStatus, error)
	exec    branchStatusExecFunc
}

func newBranchStatusCommand(deps branchStatusDependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the container's current branch and uncommitted changes",
		Long: "Show the current branch and porcelain worktree status for the container's configured workdir.\n\n" +
			"This is an inspection-only command: the container must already be running, and dv does not run lifecycle hooks or apply copy rules.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := deps.resolve(cmd)
			if err != nil {
				return err
			}
			if !deps.exists(target.name) {
				return fmt.Errorf("container %q does not exist; run 'dv start' first", target.name)
			}
			if !deps.running(target.name) {
				return fmt.Errorf("container %q is not running; run 'dv start --name %s' first", target.name, target.name)
			}

			status, err := deps.inspect(target.name, target.effectiveWorkdir, deps.exec)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Container: %s\n", target.name)
			fmt.Fprintf(out, "Branch:    %s\n", status.branch)
			if status.changes == "" {
				fmt.Fprintln(out, "Changes:   none (working tree clean)")
			} else {
				fmt.Fprintf(out, "Changes:\n%s\n", status.changes)
			}
			return nil
		},
	}
	cmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	return cmd
}

var branchStatusCmd = newBranchStatusCommand(branchStatusDependencies{
	resolve: resolveContainerTarget,
	exists:  docker.Exists,
	running: docker.Running,
	inspect: inspectBranchWorktree,
	exec:    docker.ExecOutputWithStderr,
})

func inspectBranchWorktree(containerName, workdir string, exec branchStatusExecFunc) (branchWorktreeStatus, error) {
	branch, stderr, err := exec(containerName, workdir, nil, []string{"git", "rev-parse", "--abbrev-ref", "HEAD"})
	if err != nil {
		return branchWorktreeStatus{}, branchGitError(containerName, workdir, "determine current branch", stderr, err)
	}
	branch = strings.TrimSpace(branch)
	if branch == "HEAD" {
		sha, stderr, err := exec(containerName, workdir, nil, []string{"git", "rev-parse", "--short", "HEAD"})
		if err != nil {
			return branchWorktreeStatus{}, branchGitError(containerName, workdir, "determine detached HEAD commit", stderr, err)
		}
		branch = "detached at " + strings.TrimSpace(sha)
	}

	changes, stderr, err := exec(containerName, workdir, nil, []string{"git", "--no-optional-locks", "status", "--porcelain=v1", "--untracked-files=normal"})
	if err != nil {
		return branchWorktreeStatus{}, branchGitError(containerName, workdir, "check for changes", stderr, err)
	}
	return branchWorktreeStatus{
		branch:  branch,
		changes: strings.TrimRight(changes, "\r\n"),
	}, nil
}

func branchGitError(containerName, workdir, action, stderr string, err error) error {
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		return fmt.Errorf("container %q workdir %q: failed to %s: %w", containerName, workdir, action, err)
	}
	return fmt.Errorf("container %q workdir %q: failed to %s: %s: %w", containerName, workdir, action, detail, err)
}

func init() {
	branchCmd.AddCommand(branchStatusCmd)
}
