package cli

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/localproxy"
	"dv/internal/xdg"

	"github.com/spf13/cobra"
)

var hostnameCmd = &cobra.Command{
	Use:   "hostname",
	Short: "Manage additional proxy hostnames for a container",
	Long:  "Manage additional proxy hostnames for a container. Short names use the configured\nproxy suffix; full names must be directly under that suffix. Aliases preserve the\nrequest Host for multisite routing. Settings are applied in place; running Rails\nand Caddy services may briefly restart, but the container is never recreated.",
}

func init() {
	for _, action := range []string{"add", "list", "remove"} {
		use := action + " CONTAINER HOSTNAME [HOSTNAME...]"
		args := cobra.MinimumNArgs(2)
		if action == "list" {
			use = "list CONTAINER"
			args = cobra.ExactArgs(1)
		}
		descriptions := map[string]string{"add": "Add proxy hostname aliases", "list": "List primary hostname and aliases", "remove": "Remove proxy hostname aliases"}
		hostnameCmd.AddCommand(&cobra.Command{Use: use, Short: descriptions[action], Args: args,
			ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
				if len(args) == 0 {
					return completeAgentNames(cmd, toComplete)
				}
				return nil, cobra.ShellCompDirectiveNoFileComp
			},
			RunE: func(cmd *cobra.Command, args []string) error { return runHostname(cmd, action, args) },
		})
	}
}

// Keep aliases at one level under the configured suffix, matching wildcard TLS.
func normalizeAlias(value, suffix string) (string, error) {
	suffix = strings.ToLower(strings.Trim(strings.TrimSpace(suffix), "."))
	if suffix == "" {
		suffix = "dv.localhost"
	}
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	short := strings.TrimSuffix(value, "."+suffix)
	if len(short) == 0 || len(short) > 63 || strings.Trim(short, "-") != short {
		return "", fmt.Errorf("invalid hostname %q", value)
	}
	for _, c := range short {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return "", fmt.Errorf("hostname must be a single DNS label or NAME.%s", suffix)
		}
	}
	host := short + "." + suffix
	if len(host) > 253 {
		return "", fmt.Errorf("hostname is too long")
	}
	return host, nil
}

func checkPrimaryHostname(cfg config.Config, name string) error {
	if !cfg.LocalProxy.Enabled {
		return nil
	}
	host := localproxy.HostnameForContainer(name, cfg.LocalProxy.Hostname)
	for owner, hosts := range cfg.HostnameRemovals {
		if slices.Contains(hosts, host) {
			return fmt.Errorf("hostname %s has pending cleanup for %s", host, owner)
		}
	}
	for owner, aliases := range cfg.HostnameAliases {
		if slices.Contains(aliases, host) {
			return fmt.Errorf("hostname %s is already an alias of %s", host, owner)
		}
	}
	return nil
}

func validateAliasOwner(cfg config.Config, name, host string, names []string) error {
	names = append(slices.Clone(names), name, cfg.DefaultContainer, cfg.SelectedAgent)
	for n := range cfg.HostnameAliases {
		names = append(names, n)
	}
	for n := range cfg.ContainerImages {
		names = append(names, n)
	}
	for _, n := range names {
		if n != "" && localproxy.HostnameForContainer(n, cfg.LocalProxy.Hostname) == host {
			return fmt.Errorf("%s is the primary hostname of %s", host, n)
		}
	}
	for owner, aliases := range cfg.HostnameRemovals {
		if owner != name && slices.Contains(aliases, host) {
			return fmt.Errorf("%s has pending cleanup for %s; retry its hostname removal first", host, owner)
		}
	}
	for owner, aliases := range cfg.HostnameAliases {
		if owner != name && slices.Contains(aliases, host) {
			return fmt.Errorf("%s is already an alias of %s", host, owner)
		}
	}
	return nil
}

func runHostname(cmd *cobra.Command, action string, args []string) error {
	return withHostnameOperationLock(cmd, func() error { return runHostnameLocked(cmd, action, args) })
}

func runHostnameLocked(cmd *cobra.Command, action string, args []string) error {
	dir, err := xdg.ConfigDir()
	if err != nil {
		return err
	}
	cfg, err := config.LoadOrCreate(dir)
	if err != nil {
		return err
	}
	name := args[0]
	if action == "list" {
		primary, err := containerPrimaryHostname(cfg, name)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s (primary)\n", primary)
		for _, host := range cfg.HostnameAliases[name] {
			fmt.Fprintln(cmd.OutOrStdout(), host)
		}
		return nil
	}
	if action == "add" && !cfg.LocalProxy.Enabled {
		return fmt.Errorf("enable the local proxy first with dv config local-proxy")
	}
	if action == "add" && !docker.Exists(name) {
		return fmt.Errorf("container %q does not exist; create it first", name)
	}
	primary, err := containerPrimaryHostname(cfg, name)
	containerAvailable := err == nil
	if err != nil {
		_, managed := cfg.HostnameAliases[name]
		if action != "remove" || (!managed && len(cfg.HostnameRemovals[name]) == 0) {
			return err
		}
		primary = localproxy.HostnameForContainer(name, cfg.LocalProxy.Hostname)
	}
	var names []string
	if action == "add" {
		out, err := exec.CommandContext(cmd.Context(), "docker", "ps", "-a", "--format", "{{.Names}}").Output()
		if err != nil {
			return fmt.Errorf("check hostname conflicts: %w", err)
		}
		names = strings.Fields(string(out))
	}
	err = config.Update(dir, func(latest *config.Config) error {
		hosts := slices.Clone(latest.HostnameAliases[name])
		for _, input := range args[1:] {
			host, err := normalizeAlias(input, latest.LocalProxy.Hostname)
			if err != nil {
				return err
			}
			if action == "add" {
				if err := validateAliasOwner(*latest, name, host, names); err != nil {
					return err
				}
				if latest.HostnameRemovals != nil {
					latest.HostnameRemovals[name] = slices.DeleteFunc(latest.HostnameRemovals[name], func(h string) bool { return h == host })
					if len(latest.HostnameRemovals[name]) == 0 {
						delete(latest.HostnameRemovals, name)
					}
				}
				if !slices.Contains(hosts, host) {
					hosts = append(hosts, host)
				}
			} else {
				if host == primary && !containerAvailable && len(latest.HostnameAliases[name]) == 0 && len(latest.HostnameRemovals[name]) == 0 {
					continue // Retired primary was already cleaned up.
				}
				if host == primary && !slices.Contains(latest.HostnameRemovals[name], host) {
					return fmt.Errorf("cannot remove the primary hostname")
				}
				if !slices.Contains(hosts, host) && !slices.Contains(latest.HostnameRemovals[name], host) {
					// Removing an already absent alias is a successful no-op, but
					// never delete a route that belongs to another container.
					if err := validateAliasOwner(*latest, name, host, nil); err != nil {
						return err
					}
					continue
				}
				hosts = slices.DeleteFunc(hosts, func(h string) bool { return h == host })
				queueHostnameRemovals(latest, name, host)
			}
		}
		if latest.HostnameAliases == nil {
			latest.HostnameAliases = map[string][]string{}
		}
		slices.Sort(hosts)
		// Keep an empty entry for cleanup on subsequent starts: Docker may restore
		// creation-time host entries, and the persistent service overrides still
		// need to be reconciled after a rename or recreation.
		latest.HostnameAliases[name] = hosts
		cfg = *latest
		return nil
	})
	if err != nil {
		return err
	}
	if err := config.PublishProxyAliases(dir); err != nil {
		return fmt.Errorf("hostnames saved, but publishing proxy ownership failed; retry the same command: %w", err)
	}
	var failures []error
	proxyRunning := cfg.LocalProxy.Enabled && localproxy.Running(cfg.LocalProxy)
	if proxyRunning {
		if err := reconcileHostnameRoutes(dir, cfg, name); err != nil {
			failures = append(failures, err)
		}
	}
	if docker.Running(name) {
		if err := syncContainerHostnames(cmd, cfg, name); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("hostnames saved but not fully applied; retry the same hostname command: %w", errors.Join(failures...))
	}
	if proxyRunning && docker.Running(name) {
		fmt.Fprintf(cmd.OutOrStdout(), "Hostnames applied to %s.\n", name)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Hostnames saved for %s; routing/application updates are pending until the proxy and container are running.\n", name)
	}
	if img, err := resolveImageConfig(cfg, name); err == nil && img.Kind != "discourse" {
		fmt.Fprintln(cmd.ErrOrStderr(), "Configure hostname acceptance in your custom application if needed.")
	}
	warnHostnameProxyUpgrade(cmd, cfg.LocalProxy)
	return nil
}

func warnHostnameProxyUpgrade(cmd *cobra.Command, cfg config.LocalProxyConfig) {
	if !cfg.Enabled || cfg.ContainerName == "" {
		return
	}
	// Alias-capable proxies are created with this environment variable. Only
	// advise an upgrade after successful inspection proves it is absent.
	envs, err := docker.GetContainerEnv(cfg.ContainerName)
	if err != nil || strings.TrimSpace(envs["PROXY_ALIASES_FILE"]) != "" {
		return
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "Your proxy needs an upgrade to support hostname aliases. Run: dv config local-proxy --rebuild --recreate")
}

// The removal journal makes failures retryable even after desired config has
// changed. Always try all removals, then register desired routes, reporting errors.
func cleanupHostnameRoutes(dir string, cfg config.Config, name string) error {
	var failures []error
	projectionErr := config.PublishProxyAliases(dir)
	if projectionErr != nil {
		failures = append(failures, projectionErr)
	}
	var removed []string
	for _, host := range cfg.HostnameRemovals[name] {
		if err := localproxy.RemoveRoute(cfg.LocalProxy, host); err != nil {
			failures = append(failures, fmt.Errorf("remove route %s: %w", host, err))
		} else {
			removed = append(removed, host)
		}
	}
	if len(removed) > 0 && projectionErr == nil {
		if err := config.Update(dir, func(latest *config.Config) error {
			if latest.HostnameRemovals == nil {
				return nil
			}
			latest.HostnameRemovals[name] = slices.DeleteFunc(latest.HostnameRemovals[name], func(h string) bool { return slices.Contains(removed, h) })
			if len(latest.HostnameRemovals[name]) == 0 {
				delete(latest.HostnameRemovals, name)
			}
			return nil
		}); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func reconcileHostnameRoutes(dir string, cfg config.Config, name string) error {
	cleanupErr := cleanupHostnameRoutes(dir, cfg, name)
	var registerErr error
	if docker.Running(name) {
		registerErr = registerContainerRoutes(cfg, name)
	}
	return errors.Join(cleanupErr, registerErr)
}

func queueHostnameRemovals(cfg *config.Config, name string, hosts ...string) {
	for _, host := range hosts {
		if host == "" || slices.Contains(cfg.HostnameRemovals[name], host) {
			continue
		}
		if cfg.HostnameRemovals == nil {
			cfg.HostnameRemovals = map[string][]string{}
		}
		cfg.HostnameRemovals[name] = append(cfg.HostnameRemovals[name], host)
	}
}
