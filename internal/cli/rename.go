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
	renameDockerExists        = docker.Exists
	renameDockerRenameContext = docker.RenameContext
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
	dir, err := xdg.ConfigDir()
	if err != nil {
		return err
	}
	return runRenameInConfig(cmd, oldName, newName, dir)
}

func runRenameInConfig(cmd *cobra.Command, oldName, newName, configDir string) error {
	return withHostnameOperationLockAt(cmd, configDir, func() error { return runRenameLocked(cmd, oldName, newName, configDir) })
}

func runRenameLocked(cmd *cobra.Command, oldName, newName, configDir string) error {
	operationCtx := cmd.Context()
	if operationCtx == nil {
		operationCtx = context.Background()
	}
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" || newName == "" {
		return fmt.Errorf("invalid names")
	}
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		return err
	}
	if err := checkPrimaryHostname(cfg, newName); err != nil {
		return err
	}
	renamingEnvSelection := os.Getenv("DV_AGENT") == oldName
	renamingSessionAgent := session.GetCurrentAgent() == oldName
	if !renameDockerExists(oldName) {
		return fmt.Errorf("agent '%s' does not exist", oldName)
	}
	if renameDockerExists(newName) {
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
	if err := renameDockerRenameContext(context.WithoutCancel(operationCtx), oldName, newName, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
		return err
	}
	if renamingSessionAgent {
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
		if latest.DefaultContainer == oldName {
			latest.DefaultContainer = newName
		}
		if latest.ContainerImages != nil {
			if img, ok := latest.ContainerImages[oldName]; ok {
				delete(latest.ContainerImages, oldName)
				latest.ContainerImages[newName] = img
			}
		}
		if hosts, ok := latest.HostnameAliases[oldName]; ok {
			delete(latest.HostnameAliases, oldName)
			latest.HostnameAliases[newName] = hosts
		}
		if hosts, ok := latest.HostnameRemovals[oldName]; ok {
			delete(latest.HostnameRemovals, oldName)
			queueHostnameRemovals(latest, newName, hosts...)
		}
		if proxyHost != newHost {
			queueHostnameRemovals(latest, newName, proxyHost)
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
		if !config.IsProjectionError(err) {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v\n", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Renamed agent '%s' -> '%s'\n", oldName, newName)
	if renamingEnvSelection {
		requestShellAgent(newName)
	}

	if proxyHost != "" {
		if _, managed := cfg.HostnameAliases[newName]; managed && docker.Running(newName) {
			if err := syncContainerHostnames(cmd, cfg, newName); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v\n", err)
			}
		} else if docker.Running(newName) {
			if err := renameContainerHostEntry(cmd, newName, proxyHost, newHost); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: update hosts after rename: %v\n", err)
			}
		}

		if localproxy.Running(cfg.LocalProxy) && containerPort > 0 {
			if err := reconcileHostnameRoutes(configDir, cfg, newName); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: proxy update after rename incomplete: %v (retry dv start %s)\n", err, newName)
			}
		}
		if proxyHost != newHost {
			fmt.Fprintf(cmd.ErrOrStderr(), "Proxy hostname updated: %s -> %s. Manually launched application processes may need their hostname settings updated.\n", proxyHost, newHost)
		}
	}
	return nil
}

// Docker bind-mounts /etc/hosts, so write through it rather than using sed -i.
// Pass hostnames as quoted data and match whole fields, not interpolated regexes.
func renameContainerHostEntry(cmd *cobra.Command, name, oldHost, newHost string) error {
	script := "set -eu\nold_host=" + shellQuote(oldHost) + "\nnew_host=" + shellQuote(newHost) + "\n" + `
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
awk -v old="$old_host" -v new="$new_host" '{ for(i=2;i<=NF;i++) { if($i ~ /^#/) break; if($i == old) $i=new; if($i == new) found=1 } print } END { if(!found) print "127.0.0.1 " new }' /etc/hosts > "$tmp"
cat "$tmp" > /etc/hosts
`
	ctx := context.Background()
	if cmd != nil && cmd.Context() != nil {
		ctx = cmd.Context()
	}
	_, err := docker.ExecAsRootScriptContext(ctx, name, "/", script)
	return err
}
