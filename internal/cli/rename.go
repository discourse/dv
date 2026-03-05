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

var renameCmd = &cobra.Command{
	Use:   "rename OLD NEW",
	Short: "Rename an existing agent container",
	Args:  cobra.ExactArgs(2),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Complete OLD when providing the first arg, NEW is free text
		if len(args) == 0 {
			return completeAgentNames(cmd, toComplete)
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		oldName := strings.TrimSpace(args[0])
		newName := strings.TrimSpace(args[1])
		if oldName == "" || newName == "" {
			return fmt.Errorf("invalid names")
		}
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}
		if !docker.Exists(oldName) {
			return fmt.Errorf("agent '%s' does not exist", oldName)
		}
		if docker.Exists(newName) {
			return fmt.Errorf("an agent named '%s' already exists", newName)
		}
		var proxyHost string
		var containerPort int
		if cfg.LocalProxy.Enabled {
			if labels, err := labelsWithOverrides(oldName, cfg); err == nil {
				if host, _, cp, _, ok := localproxy.RouteFromLabels(labels); ok {
					proxyHost = host
					containerPort = cp
				}
			}
		}
		if err := docker.Rename(oldName, newName); err != nil {
			return err
		}
		// Update selection and mappings
		if cfg.SelectedAgent == oldName {
			cfg.SelectedAgent = newName
		}
		if session.GetCurrentAgent() == oldName {
			_ = session.SetCurrentAgent(newName)
		}
		if cfg.ContainerImages != nil {
			if img, ok := cfg.ContainerImages[oldName]; ok {
				delete(cfg.ContainerImages, oldName)
				cfg.ContainerImages[newName] = img
			}
		}
		if cfg.CustomWorkdirs != nil {
			if w, ok := cfg.CustomWorkdirs[oldName]; ok {
				delete(cfg.CustomWorkdirs, oldName)
				cfg.CustomWorkdirs[newName] = w
			}
		}
		// Migrate label overrides from old name to new name
		if cfg.LabelOverrides != nil {
			if ov, ok := cfg.LabelOverrides[oldName]; ok {
				delete(cfg.LabelOverrides, oldName)
				cfg.LabelOverrides[newName] = ov
			}
		}

		var newHost string
		if proxyHost != "" {
			newHost = localproxy.HostnameForContainer(newName, cfg.LocalProxy.Hostname)
			// Store updated hostname as a label override since docker rename
			// doesn't update labels.
			if cfg.LabelOverrides == nil {
				cfg.LabelOverrides = map[string]map[string]string{}
			}
			if cfg.LabelOverrides[newName] == nil {
				cfg.LabelOverrides[newName] = map[string]string{}
			}
			cfg.LabelOverrides[newName][localproxy.LabelHost] = newHost
		}

		if err := config.Save(configDir, cfg); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Renamed agent '%s' -> '%s'\n", oldName, newName)

		if proxyHost != "" {

			// Update /etc/hosts inside the container if it's running
			if docker.Running(newName) {
				// Use sed to replace the old host entry or append if it doesn't exist.
				// We use \b for word boundaries to avoid partial matches.
				cmdLine := []string{"bash", "-c", fmt.Sprintf(
					"sed -i 's/\\b%s\\b/%s/g' /etc/hosts; grep -q '\\b%s\\b' /etc/hosts || echo '127.0.0.1 %s' >> /etc/hosts",
					proxyHost, newHost, newHost, newHost,
				)}
				_, _ = docker.ExecAsRoot(newName, "/", nil, cmdLine)
			}

			if localproxy.Running(cfg.LocalProxy) && containerPort > 0 {
				_ = localproxy.RemoveRoute(cfg.LocalProxy, proxyHost)
				registerWithLocalProxy(cmd, cfg, newName, newHost, containerPort)
			}
			if proxyHost != newHost {
				fmt.Fprintf(cmd.ErrOrStderr(), "Proxy hostname updated: %s -> %s. Restart with --reset if assets still point to the old name.\n", proxyHost, newHost)
			}
		}
		return nil
	},
}
