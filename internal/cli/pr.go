package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

// prCmd implements: dv pr [--name NAME] NUMBER
// - Checks out the given GitHub PR in the container's repo workdir
// - Resets DB and runs migrations and seed (mirrors Dockerfile init)
var prCmd = &cobra.Command{
	Use:   "pr [--name NAME] NUMBER",
	Short: "Checkout a PR in the container and reset DB",
	Args:  cobra.ExactArgs(1),
	// Dynamic completion: list recent PRs with titles and filter by text
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Only complete the first positional arg (PR number)
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		// Load config to determine container and workdir
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		name, _ := cmd.Flags().GetString("name")
		if name == "" {
			name = currentAgentName(cfg)
		}

		// Determine repo owner/name from container remotes (prefer upstream for forks)
		owner, repo := prSearchOwnerRepoFromContainer(cfg, name)
		if owner == "" || repo == "" {
			// Fallback to configured discourse repo
			owner, repo = ownerRepoFromURL(cfg.DiscourseRepo)
		}
		return SuggestPRNumbers(owner, repo, toComplete)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Load config and container details
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}

		// Parse PR number or search query
		prNumber, err := ResolvePR(cmd, cfg, strings.TrimSpace(args[0]))
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
			return fmt.Errorf("'dv pr' is only supported for discourse image kind; current: %q", imgCfg.Kind)
		}

		// Determine owner/repo for fetching PR details
		owner, repo := prSearchOwnerRepoFromContainer(cfg, name)
		if owner == "" || repo == "" {
			// Fallback to configured discourse repo
			owner, repo = ownerRepoFromURL(cfg.DiscourseRepo)
		}
		if owner == "" || repo == "" {
			return fmt.Errorf("unable to determine repository owner/name for fetching PR details")
		}

		// Fetch PR details to get the actual branch name
		fmt.Fprintf(cmd.OutOrStdout(), "Fetching PR #%d details from GitHub...\n", prNumber)
		prDetail, err := fetchPRDetail(owner, repo, prNumber)
		if err != nil {
			return fmt.Errorf("failed to fetch PR details: %w", err)
		}

		branchName := prDetail.Head.Ref
		if branchName == "" {
			return fmt.Errorf("PR #%d has no branch name (head.ref is empty)", prNumber)
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Checking out PR #%d (%s) in container '%s'...\n", prNumber, branchName, name)

		// Build shell script to fetch and checkout PR branch using the actual branch name
		checkoutCmds := buildPRCheckoutCommands(prNumber, branchName)
		script := buildDiscourseResetScript(checkoutCmds, discourseResetScriptOpts{})

		// Run interactively to stream output to the user
		argv := []string{"bash", "-lc", script}
		if err := docker.ExecInteractive(name, workdir, nil, argv); err != nil {
			return fmt.Errorf("container: failed to checkout PR and migrate: %w", err)
		}
		return nil
	},
}

func init() {
	prCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	rootCmd.AddCommand(prCmd)
}
