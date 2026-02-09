package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

// resetCmd is the parent command for reset operations.
// Running without a subcommand defaults to database reset (backward compatible).
var resetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Reset container state (databases or git)",
	RunE:  resetDbRunE,
}

// resetDbCmd resets databases only (no code changes).
var resetDbCmd = &cobra.Command{
	Use:   "db",
	Short: "Reset databases only (no code changes)",
	RunE:  resetDbRunE,
}

// resetGitCmd discards local changes and syncs with the upstream branch.
var resetGitCmd = &cobra.Command{
	Use:   "git",
	Short: "Discard local changes and sync with upstream branch",
	RunE:  resetGitRunE,
}

// resetDbRunE implements database-only reset.
func resetDbRunE(cmd *cobra.Command, args []string) error {
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
		return fmt.Errorf("'dv reset' is only supported for discourse image kind; current: %q", imgCfg.Kind)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Resetting databases in container '%s'...\n", name)

	script := buildDiscourseDatabaseResetScript()
	argv := []string{"bash", "-lc", script}
	if err := docker.ExecInteractive(name, workdir, nil, argv); err != nil {
		return fmt.Errorf("container: failed to reset databases: %w", err)
	}
	return nil
}

// resetGitRunE discards local changes and resets to origin/<current-branch>.
func resetGitRunE(cmd *cobra.Command, args []string) error {
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
		return fmt.Errorf("'dv reset git' is only supported for discourse image kind; current: %q", imgCfg.Kind)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Resetting git and migrating in container '%s'...\n", name)

	script := buildDiscourseResetScript(buildCurrentBranchResetCommands(), discourseResetScriptOpts{})
	argv := []string{"bash", "-lc", script}
	if err := docker.ExecInteractive(name, workdir, nil, argv); err != nil {
		return fmt.Errorf("container: failed to reset git: %w", err)
	}
	return nil
}

func init() {
	resetCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	resetDbCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	resetGitCmd.Flags().String("name", "", "Container name (defaults to selected or default)")

	resetCmd.AddCommand(resetDbCmd)
	resetCmd.AddCommand(resetGitCmd)
}
