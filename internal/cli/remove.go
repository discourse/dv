package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/localproxy"
	"dv/internal/session"
	"dv/internal/xdg"
)

var (
	removeDockerExists      = docker.Exists
	removeDockerRunning     = docker.Running
	removeDockerRemove      = docker.Remove
	removeDockerRemoveForce = docker.RemoveForce
	removeDockerImageExists = docker.ImageExists
	removeDockerRemoveImage = docker.RemoveImage
)

type partialOperationError struct {
	action string
	err    error
}

func (e *partialOperationError) Error() string {
	return fmt.Sprintf("%s completed with warning: %v", e.action, e.err)
}

func (e *partialOperationError) Unwrap() error { return e.err }

var removeCmd = &cobra.Command{
	Use:     "remove [NAME]",
	Aliases: []string{"rm"},
	Short:   "Remove container and optionally its image",
	Args:    cobra.RangeArgs(0, 1),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Complete NAME
		if len(args) == 0 {
			return completeAgentNames(cmd, toComplete)
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		force, _ := cmd.Flags().GetBool("force")
		return runRemove(cmd, args, force, false)
	},
}

func runRemove(cmd *cobra.Command, args []string, force, directOutput bool) error {
	operationCtx := cmd.Context()
	if operationCtx == nil {
		operationCtx = context.Background()
	}
	configDir, err := xdg.ConfigDir()
	if err != nil {
		return err
	}
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		return err
	}
	removeImage, _ := cmd.Flags().GetBool("image")
	name, _ := cmd.Flags().GetString("name")
	if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
		name = args[0]
	}
	if name == "" {
		name = currentAgentName(cfg)
	}
	removingEffectiveSelection := currentAgentName(cfg) == name
	removingEnvSelection := os.Getenv("DV_AGENT") == name
	removingSessionSelection := session.GetCurrentAgent() == name
	imgForContainer := cfg.ContainerImages[name]
	var proxyHost string
	if cfg.LocalProxy.Enabled {
		if labels, err := labelsWithOverrides(name, cfg); err == nil {
			if host, _, _, _, ok := localproxy.RouteFromLabels(labels); ok {
				proxyHost = host
			}
		}
	}

	removalHookCtx := hostHookContext{
		CommandName:   cmd.Name(),
		ContainerName: name,
		ImageName:     imgForContainer,
		ConfigDir:     configDir,
	}

	containerRemoved := false
	var removeErr error
	if removeDockerExists(name) {
		if proceed, err := warnActiveSessions(cmd, name, force); err != nil {
			return err
		} else if !proceed {
			return nil
		}

		removalHookCtx = enrichHostHookContextForContainer(cfg, hostHookPreRemove, removalHookCtx)
		if err := runConfiguredHostHooks(cmd, cfg, hostHookPreRemove, removalHookCtx); err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Stopping and removing container '%s'...\n", name)
		if directOutput {
			// Once confirmed, let removal and config cleanup complete together. Killing
			// the Docker CLI mid-request can remove the container but skip cleanup.
			removeCtx := context.WithoutCancel(operationCtx)
			if removeDockerRunning(name) {
				removeErr = docker.RemoveForceContext(removeCtx, name, cmd.OutOrStdout(), cmd.ErrOrStderr())
			} else {
				removeErr = docker.RemoveContext(removeCtx, name, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
		} else if removeDockerRunning(name) {
			removeErr = removeDockerRemoveForce(name)
		} else {
			removeErr = removeDockerRemove(name)
		}
		containerRemoved = removeErr == nil
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Container '%s' does not exist\n", name)
	}
	if removeErr != nil {
		return fmt.Errorf("remove container %q: %w", name, removeErr)
	}

	if removeImage {
		if removeDockerImageExists(cfg.ImageTag) {
			fmt.Fprintf(cmd.OutOrStdout(), "Removing image '%s'...\n", cfg.ImageTag)
			_ = removeDockerRemoveImage(cfg.ImageTag)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "Image '%s' does not exist\n", cfg.ImageTag)
		}
	}

	var replacement string
	imageName := imgForContainer
	if imageName == "" {
		imageName = cfg.SelectedImage
	}
	if resolvedName, imageCfg, resolveErr := resolveImage(cfg, imageName); resolveErr == nil {
		records, inventoryErr := collectContainerInventory(context.WithoutCancel(operationCtx), cfg, resolvedName, imageCfg, "", nil)
		if inventoryErr == nil {
			for _, record := range records {
				if record.name == name {
					continue
				}
				if replacement == "" {
					replacement = record.name
				}
				if record.status == "Running" {
					replacement = record.name
					break
				}
			}
		}
	}

	var replacementSelected bool
	if err := config.Update(configDir, func(latest *config.Config) error {
		delete(latest.ContainerImages, name)
		delete(latest.LabelOverrides, name)
		delete(latest.CustomWorkdirs, name)
		if latest.SelectedAgent == name {
			replacementSelected = true
			latest.SelectedAgent = replacement
		}
		if latest.DefaultContainer == name {
			latest.DefaultContainer = replacement
		}
		cfg = *latest
		return nil
	}); err != nil {
		return err
	}
	fallbackSelection := cfg.SelectedAgent
	if fallbackSelection == "" {
		fallbackSelection = cfg.DefaultContainer
	}
	if fallbackSelection == name {
		fallbackSelection = replacement
	}
	if removingSessionSelection {
		_ = session.SetCurrentAgent(fallbackSelection)
	}
	effectiveFallback := fallbackSelection
	if removingEnvSelection {
		if sessionSelection := session.GetCurrentAgent(); sessionSelection != "" {
			effectiveFallback = sessionSelection
		}
	}
	selectionAffected := replacementSelected || removingEffectiveSelection
	if selectionAffected {
		if effectiveFallback != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "Selected agent: %s\n", effectiveFallback)
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "Selected agent: (none)")
		}
	}
	if removingEffectiveSelection {
		// Drop the shell override so normal session -> global -> default
		// precedence determines the next effective selection.
		requestShellAgent("")
	}

	if proxyHost != "" && localproxy.Running(cfg.LocalProxy) {
		if err := localproxy.RemoveRoute(cfg.LocalProxy, proxyHost); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not remove %s from local proxy: %v\n", proxyHost, err)
		}
	}

	if containerRemoved {
		if err := runConfiguredHostHooks(cmd, cfg, hostHookPostRemove, removalHookCtx); err != nil {
			return &partialOperationError{action: "remove", err: err}
		}
	}

	fmt.Fprintln(cmd.OutOrStdout(), "Removal complete")
	return nil
}

func init() {
	removeCmd.Flags().Bool("image", false, "Also remove the Docker image after removing container")
	removeCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	removeCmd.Flags().BoolP("force", "f", false, "Skip active session warning")
}
