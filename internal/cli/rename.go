package cli

import (
	"context"
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
		if len(args) == 0 {
			return completeAgentNames(cmd, toComplete)
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRename(cmd, args[0], args[1])
	},
}

func runRename(cmd *cobra.Command, oldName, newName string) error {
	operationCtx := cmd.Context()
	if operationCtx == nil {
		operationCtx = context.Background()
	}
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
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
	// Rename is intentionally allowed to finish once dispatched; interrupting the
	// Docker CLI after the daemon accepts the rename can desynchronize config.
	if err := docker.RenameContext(context.WithoutCancel(operationCtx), oldName, newName, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
		return err
	}
	if session.GetCurrentAgent() == oldName {
		_ = session.SetCurrentAgent(newName)
	}
	var newHost string
	if proxyHost != "" {
		newHost = localproxy.HostnameForContainer(newName, cfg.LocalProxy.Hostname)
	}
	if err := config.Update(configDir, func(latest *config.Config) error {
		if latest.SelectedAgent == oldName {
			latest.SelectedAgent = newName
		}
		if latest.ContainerImages != nil {
			if img, ok := latest.ContainerImages[oldName]; ok {
				delete(latest.ContainerImages, oldName)
				latest.ContainerImages[newName] = img
			}
		}
		if latest.CustomWorkdirs != nil {
			if workdir, ok := latest.CustomWorkdirs[oldName]; ok {
				delete(latest.CustomWorkdirs, oldName)
				latest.CustomWorkdirs[newName] = workdir
			}
		}
		if latest.LabelOverrides != nil {
			if overrides, ok := latest.LabelOverrides[oldName]; ok {
				delete(latest.LabelOverrides, oldName)
				latest.LabelOverrides[newName] = overrides
			}
		}
		if newHost != "" {
			if latest.LabelOverrides == nil {
				latest.LabelOverrides = map[string]map[string]string{}
			}
			if latest.LabelOverrides[newName] == nil {
				latest.LabelOverrides[newName] = map[string]string{}
			}
			latest.LabelOverrides[newName][localproxy.LabelHost] = newHost
		}
		cfg = *latest
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Renamed agent '%s' -> '%s'\n", oldName, newName)

	if proxyHost != "" {
		if docker.Running(newName) {
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
}
