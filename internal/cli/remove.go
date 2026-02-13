package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/localproxy"
	"dv/internal/session"
	"dv/internal/xdg"
)

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
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}
		dirty := false

		removeImage, _ := cmd.Flags().GetBool("image")
		name, _ := cmd.Flags().GetString("name")
		if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
			name = args[0]
		}
		if name == "" {
			name = currentAgentName(cfg)
		}
		imgForContainer := cfg.ContainerImages[name]
		var proxyHost string
		if cfg.LocalProxy.Enabled {
			if labels, err := docker.Labels(name); err == nil {
				if host, _, _, _, ok := localproxy.RouteFromLabels(labels); ok {
					proxyHost = host
				}
			}
		}

		if docker.Exists(name) {
			force, _ := cmd.Flags().GetBool("force")
			if proceed, err := warnActiveSessions(cmd, name, force); err != nil {
				return err
			} else if !proceed {
				return nil
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Stopping and removing container '%s'...\n", name)
			if docker.Running(name) {
				_ = docker.RemoveForce(name)
			} else {
				_ = docker.Remove(name)
			}
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "Container '%s' does not exist\n", name)
		}

		if removeImage {
			if docker.ImageExists(cfg.ImageTag) {
				fmt.Fprintf(cmd.OutOrStdout(), "Removing image '%s'...\n", cfg.ImageTag)
				_ = docker.RemoveImage(cfg.ImageTag)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Image '%s' does not exist\n", cfg.ImageTag)
			}
		}

		if cfg.ContainerImages != nil {
			if _, ok := cfg.ContainerImages[name]; ok {
				delete(cfg.ContainerImages, name)
				dirty = true
			}
		}
		if cfg.CustomWorkdirs != nil {
			if _, ok := cfg.CustomWorkdirs[name]; ok {
				delete(cfg.CustomWorkdirs, name)
				dirty = true
			}
		}

		// If we removed the selected agent, choose the first remaining container for the selected image
		if cfg.SelectedAgent == name {
			// Determine image to filter by: prefer the container's recorded image, else the currently selected image
			imgName := imgForContainer
			_, imgCfg, err := resolveImage(cfg, imgName)
			if err != nil {
				// Fallback to selected image silently
				_, imgCfg, _ = resolveImage(cfg, "")
			}

			out, _ := runShell("docker ps -a --format '{{.Names}}\t{{.Image}}'")
			var first string
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				parts := strings.SplitN(line, "\t", 2)
				if len(parts) < 2 {
					continue
				}
				n, image := parts[0], parts[1]
				if image != imgCfg.Tag {
					continue
				}
				first = n
				break
			}
			cfg.SelectedAgent = first
			if session.GetCurrentAgent() == name {
				_ = session.SetCurrentAgent(first)
			}
			dirty = true
			if first != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Selected agent: %s\n", first)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Selected agent: (none)")
			}
		}

		if dirty {
			if err := config.Save(configDir, cfg); err != nil {
				return err
			}
		}

		if proxyHost != "" && localproxy.Running(cfg.LocalProxy) {
			if err := localproxy.RemoveRoute(cfg.LocalProxy, proxyHost); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not remove %s from local proxy: %v\n", proxyHost, err)
			}
		}

		fmt.Fprintln(cmd.OutOrStdout(), "Removal complete")
		return nil
	},
}

func init() {
	removeCmd.Flags().Bool("image", false, "Also remove the Docker image after removing container")
	removeCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	removeCmd.Flags().BoolP("force", "f", false, "Skip active session warning")
}
