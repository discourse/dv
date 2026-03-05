package cli

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

var startCmd = &cobra.Command{
	Use:   "start [name] [--reset] [--no-remap] [--image NAME] [--host-starting-port N] [--container-port N]",
	Short: "Create or start a container for the selected image",
	Args:  cobra.MaximumNArgs(1),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Complete container name for the first positional argument
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

		reset, _ := cmd.Flags().GetBool("reset")
		// Priority: positional arg > --name flag > config
		name, _ := cmd.Flags().GetString("name")
		if len(args) > 0 {
			name = args[0]
		}
		imageOverride, _ := cmd.Flags().GetString("image")
		if name == "" {
			name = currentAgentName(cfg)
		}

		hostPort, _ := cmd.Flags().GetInt("host-starting-port")
		containerPort, _ := cmd.Flags().GetInt("container-port")
		if hostPort == 0 {
			hostPort = cfg.HostStartingPort
		}
		if containerPort == 0 {
			containerPort = cfg.ContainerPort
		}

		// Determine which image and workdir to use from image selection
		imgName, imgCfg, err := resolveImage(cfg, imageOverride)
		if err != nil {
			return err
		}
		imageTag := imgCfg.Tag
		workdir := imgCfg.Workdir

		if reset && docker.Exists(name) {
			fmt.Fprintf(cmd.OutOrStdout(), "Stopping and removing existing container '%s'...\n", name)
			_ = docker.Stop(name)
			_ = docker.Remove(name)
		}

		overridesDirty := false
		if reset || !docker.Exists(name) {
			// Clear label overrides — fresh container gets correct labels
			if _, ok := cfg.LabelOverrides[name]; ok {
				delete(cfg.LabelOverrides, name)
				overridesDirty = true
			}
		}

		if !docker.Exists(name) {
			// Find the first available host port, starting from hostPort
			allocated, err := docker.AllocatedPorts()
			if err != nil {
				if isTruthyEnv("DV_VERBOSE") {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to detect allocated Docker ports: %v\n", err)
				}
			}
			chosenPort := hostPort
			if isTruthyEnv("DV_VERBOSE") {
				fmt.Fprintf(cmd.OutOrStdout(), "Searching for an available port starting from %d...\n", chosenPort)
			}
			for isPortInUse(chosenPort, allocated) {
				chosenPort++
			}
			if isTruthyEnv("DV_VERBOSE") {
				fmt.Fprintf(cmd.OutOrStdout(), "Selected port %d.\n", chosenPort)
			}
			if chosenPort != hostPort {
				fmt.Fprintf(cmd.OutOrStdout(), "Port %d in use, using %d.\n", hostPort, chosenPort)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Creating and starting container '%s' with image '%s'...\n", name, imageTag)
			labels := map[string]string{
				"com.dv.owner":      "dv",
				"com.dv.image-name": imgName,
				"com.dv.image-tag":  imageTag,
			}
			envs := map[string]string{
				"DISCOURSE_PORT": strconv.Itoa(chosenPort),
			}
			extraHosts := []string{}
			proxyHost := applyLocalProxyMetadata(cfg, name, chosenPort, containerPort, labels, envs)
			if proxyHost != "" {
				extraHosts = append(extraHosts, fmt.Sprintf("%s:127.0.0.1", proxyHost))
			}
			if err := docker.RunDetached(name, workdir, imageTag, chosenPort, containerPort, labels, envs, extraHosts, ""); err != nil {
				return err
			}

			// give it a moment to boot services
			time.Sleep(500 * time.Millisecond)

			if proxyHost != "" {
				registerWithLocalProxy(cmd, cfg, name, proxyHost, containerPort)
			}
		} else if !docker.Running(name) {
			// Check if container's port is available before starting
			existingPort, portErr := docker.GetContainerHostPort(name, containerPort)
			noRemap, _ := cmd.Flags().GetBool("no-remap")

			allocated, _ := docker.AllocatedPorts()
			if portErr == nil && existingPort > 0 {
				// Remove our own port from the check to avoid false positive remapping
				delete(allocated, existingPort)

				if isPortInUse(existingPort, allocated) {
					if noRemap {
						return fmt.Errorf("port %d is in use; free the port, use --reset to recreate, or remove --no-remap to auto-remap", existingPort)
					}

					// Find next available port
					newPort := existingPort
					for isPortInUse(newPort, allocated) {
						newPort++
					}

					fmt.Fprintf(cmd.OutOrStdout(), "Port %d in use, remapping to %d...\n", existingPort, newPort)

					// Get container metadata for recreation
					labels, _ := labelsWithOverrides(name, cfg)
					existingWorkdir, _ := docker.GetContainerWorkdir(name)
					if existingWorkdir == "" {
						existingWorkdir = workdir
					}
					existingEnvs, _ := docker.GetContainerEnv(name)

					// Commit container to temporary image
					tempImage := name + "-dv-snapshot"
					fmt.Fprintf(cmd.OutOrStdout(), "Saving container state...\n")
					if err := docker.CommitContainer(name, tempImage); err != nil {
						return fmt.Errorf("failed to snapshot container: %w", err)
					}

					// Remove old container
					if err := docker.Remove(name); err != nil {
						_ = docker.RemoveImage(tempImage)
						return fmt.Errorf("failed to remove old container: %w", err)
					}

					// Update DISCOURSE_PORT env if present
					if existingEnvs == nil {
						existingEnvs = make(map[string]string)
					}
					existingEnvs["DISCOURSE_PORT"] = fmt.Sprintf("%d", newPort)

					// Recreate container with new port from snapshot
					fmt.Fprintf(cmd.OutOrStdout(), "Recreating container with new port...\n")
					if err := docker.RunDetached(name, existingWorkdir, tempImage, newPort, containerPort, labels, existingEnvs, nil, ""); err != nil {
						// Try to restore from snapshot
						fmt.Fprintf(cmd.ErrOrStderr(), "Failed to recreate, attempting restore...\n")
						_ = docker.RunDetached(name, existingWorkdir, tempImage, existingPort, containerPort, labels, existingEnvs, nil, "")
						_ = docker.RemoveImage(tempImage)
						return fmt.Errorf("failed to recreate container: %w", err)
					}

					// Clean up snapshot image (force+quiet since new container references it)
					_ = docker.RemoveImageQuiet(tempImage)

					// Update proxy registration if needed
					proxyHost := applyLocalProxyMetadata(cfg, name, newPort, containerPort, labels, existingEnvs)
					time.Sleep(500 * time.Millisecond)
					if proxyHost != "" {
						registerWithLocalProxy(cmd, cfg, name, proxyHost, containerPort)
					}
				} else {
					// Port is free, start normally
					fmt.Fprintf(cmd.OutOrStdout(), "Starting existing container '%s'...\n", name)
					if err := docker.Start(name); err != nil {
						return err
					}
				}
			} else {
				// Couldn't determine port, start normally
				fmt.Fprintf(cmd.OutOrStdout(), "Starting existing container '%s'...\n", name)
				if err := docker.Start(name); err != nil {
					return err
				}
			}
			registerContainerFromLabels(cmd, cfg, name)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "Container '%s' is already running.\n", name)
			registerContainerFromLabels(cmd, cfg, name)
		}

		// Remember container->image association
		if cfg.ContainerImages == nil {
			cfg.ContainerImages = map[string]string{}
		}
		if cfg.ContainerImages[name] != imgName {
			cfg.ContainerImages[name] = imgName
			overridesDirty = true
		}
		if overridesDirty {
			_ = config.Save(configDir, cfg)
		}

		fmt.Fprintln(cmd.OutOrStdout(), "Ready.")
		return nil
	},
}

func init() {
	startCmd.Flags().Bool("reset", false, "Stop and remove existing container before starting fresh")
	startCmd.Flags().Bool("no-remap", false, "Fail instead of auto-remapping when port is in use")
	startCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	startCmd.Flags().Int("host-starting-port", 0, "First host port to try for container port mapping")
	startCmd.Flags().Int("container-port", 0, "Container port to expose")
	startCmd.Flags().String("image", "", "Override image to start (defaults to selected image)")
}
