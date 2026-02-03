package cli

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

// shellExecCommand is an alias for exec.Command to make testing easier
var shellExecCommand = exec.Command

// branchCmd implements: dv branch [--name NAME] BRANCH
// - Checks out the given branch in the container's repo workdir
// - Resets DB and runs migrations and seed (mirrors Dockerfile init) unless --no-reset is specified
// - Creates a new branch if --new is specified and the branch does not exist on remote
var branchCmd = &cobra.Command{
	Use:   "branch [--name NAME] [--no-reset] [--new] BRANCH",
	Short: "Checkout a branch in the container and reset DB",
	Args:  cobra.RangeArgs(0, 1),
	// Dynamic completion: list branches from discourse/discourse GitHub repo
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Only complete the first positional arg (branch name)
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		// Use git ls-remote with pattern for efficient server-side filtering
		q := strings.TrimSpace(toComplete)
		branches, err := listBranchesWithGitLsRemote("https://github.com/discourse/discourse.git", q)
		if err != nil || len(branches) == 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		return branches, cobra.ShellCompDirectiveNoFileComp
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Parse branch name
		branchName := strings.TrimSpace(args[0])
		if branchName == "" {
			return fmt.Errorf("invalid branch name: %q", args[0])
		}

		// Load config and container details
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

		// Determine workdir from associated image
		imgName := cfg.ContainerImages[name]
		var imgCfg config.ImageConfig
		if imgName != "" {
			imgCfg = cfg.Images[imgName]
		} else {
			_, imgCfg, err = resolveImage(cfg, "")
			if err != nil {
				return err
			}
		}
		workdir := imgCfg.Workdir
		if strings.TrimSpace(workdir) == "" {
			workdir = "/var/www/discourse"
		}
		if imgCfg.Kind != "discourse" {
			return fmt.Errorf("'dv branch' is only supported for discourse image kind; current: %q", imgCfg.Kind)
		}

		noReset, _ := cmd.Flags().GetBool("no-reset")
		useNew, _ := cmd.Flags().GetBool("new")

		// If --new is specified and the branch does not exist on remote, create it from origin/main (or origin/master),
		// otherwise checkout the branch, which will fail if the branch does not exist on remote.
		var checkoutCmds []string
		if useNew {
			branches, err := listBranchesWithGitLsRemote("https://github.com/discourse/discourse.git", "")
			if err != nil {
				return fmt.Errorf("listing remote branches: %w", err)
			}
			exists := slices.Contains(branches, branchName)
			if !exists {
				checkoutCmds = buildNewBranchCheckoutCommands(branchName)
				fmt.Fprintf(cmd.OutOrStdout(), "Branch '%s' not on remote; creating new branch in container '%s'...\n", branchName, name)
			} else {
				checkoutCmds = buildBranchCheckoutCommands(branchName)
				fmt.Fprintf(cmd.OutOrStdout(), "Checking out branch '%s' in container '%s'...\n", branchName, name)
			}
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "Checking out branch '%s' in container '%s'...\n", branchName, name)
			checkoutCmds = buildBranchCheckoutCommands(branchName)
		}

		// Build shell script to checkout branch safely
		script := buildDiscourseResetScript(checkoutCmds, discourseResetScriptOpts{SkipDBReset: noReset})

		// Run interactively to stream output to the user
		argv := []string{"bash", "-lc", script}
		if err := docker.ExecInteractive(name, workdir, nil, argv); err != nil {
			return fmt.Errorf("container: failed to checkout branch and migrate: %w", err)
		}
		return nil
	},
}

func init() {
	branchCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	branchCmd.Flags().Bool("new", false, "If the branch does not exist on remote, create it from origin/main (or origin/master)")
	branchCmd.Flags().Bool("no-reset", false, "Do not reset DB or run migrations; only checkout and reinstall deps")
	rootCmd.AddCommand(branchCmd)
}

// listBranchesWithGitLsRemote uses git ls-remote to fetch branches matching a pattern.
// This is much more efficient than the GitHub API as it supports server-side filtering.
// Returns a list of branch names, with main/master branches first.
func listBranchesWithGitLsRemote(repoURL, pattern string) ([]string, error) {
	// Build git ls-remote command with pattern
	// If pattern is empty, use "*" to match all branches
	// If pattern is provided, use it as a prefix match
	refPattern := "refs/heads/*"
	if pattern != "" {
		refPattern = fmt.Sprintf("refs/heads/%s*", pattern)
	}

	// Execute git ls-remote
	cmdStr := fmt.Sprintf("git ls-remote --heads %s '%s' 2>/dev/null | awk '{print $2}' | sed 's|refs/heads/||' | sort", repoURL, refPattern)
	cmd := shellExecCommand("bash", "-c", cmdStr)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var mainBranches []string
	var otherBranches []string

	for _, line := range lines {
		branch := strings.TrimSpace(line)
		if branch == "" {
			continue
		}
		if branch == "main" || branch == "master" {
			mainBranches = append(mainBranches, branch)
		} else {
			otherBranches = append(otherBranches, branch)
		}
	}

	// Put main/master first
	var result []string
	result = append(result, mainBranches...)
	result = append(result, otherBranches...)

	return result, nil
}
