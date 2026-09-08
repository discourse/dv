package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/localproxy"
)

func applyLocalProxyMetadata(cfg config.Config, containerName string, hostPort int, containerPort int, labels map[string]string, envs map[string]string) string {
	if !cfg.LocalProxy.Enabled || hostPort <= 0 {
		return ""
	}
	lp := cfg.LocalProxy
	lp.ApplyDefaults()
	if !localproxy.Running(lp) {
		return ""
	}

	host := localproxy.HostnameForContainer(containerName, lp.Hostname)
	labels[localproxy.LabelEnabled] = "true"
	labels[localproxy.LabelHost] = host
	labels[localproxy.LabelTargetPort] = strconv.Itoa(hostPort)
	labels[localproxy.LabelContainerPort] = strconv.Itoa(containerPort)
	labels[localproxy.LabelHTTPPort] = strconv.Itoa(lp.HTTPPort)
	if lp.HTTPS {
		labels[localproxy.LabelHTTPSPort] = strconv.Itoa(lp.HTTPSPort)
	}

	envs["DISCOURSE_HOSTNAME"] = host
	hosts := append([]string{host}, cfg.HostnameAliases[containerName]...)
	envs["RAILS_DEVELOPMENT_HOSTS"] = strings.Join(hosts, ",")
	envs["DV_LOCAL_PROXY_HOST"] = host
	envs["DV_LOCAL_PROXY_BASE_HOST"] = lp.Hostname
	envs["DV_CADDY_HOSTS"] = caddyHostsForLocalProxy(strings.Join(hosts, ", "), lp.Hostname)
	envs["DV_LOCAL_PROXY_HTTP_PORT"] = strconv.Itoa(lp.HTTPPort)
	if lp.HTTPS {
		envs["DV_LOCAL_PROXY_SCHEME"] = "https"
		envs["DV_LOCAL_PROXY_PORT"] = strconv.Itoa(lp.HTTPSPort)
		envs["DV_LOCAL_PROXY_HTTPS_PORT"] = strconv.Itoa(lp.HTTPSPort)
		envs["DISCOURSE_FORCE_HTTPS"] = "true"
		envs["DISCOURSE_DEV_ALLOW_HTTPS"] = "1"
		// Override DISCOURSE_PORT so URLs use the external HTTPS port; use DiscoursePort when a different port is needed
		discoursePort := lp.HTTPSPort
		if lp.DiscoursePort > 0 {
			discoursePort = lp.DiscoursePort
		}
		envs["DISCOURSE_PORT"] = strconv.Itoa(discoursePort)
	} else {
		envs["DV_LOCAL_PROXY_SCHEME"] = "http"
		envs["DV_LOCAL_PROXY_PORT"] = strconv.Itoa(lp.HTTPPort)
		// Override DISCOURSE_PORT so URLs use the external HTTP port; use DiscoursePort when a different port is needed
		discoursePort := lp.HTTPPort
		if lp.DiscoursePort > 0 {
			discoursePort = lp.DiscoursePort
		}
		envs["DISCOURSE_PORT"] = strconv.Itoa(discoursePort)
	}

	return host
}

func caddyHostsForLocalProxy(host, baseHost string) string {
	hosts := []string{}
	seen := map[string]struct{}{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		hosts = append(hosts, value)
	}

	for _, value := range strings.Split(host, ",") {
		add(value)
	}
	baseHost = strings.TrimSpace(baseHost)
	if baseHost != "" && strings.TrimPrefix(baseHost, "*.") != "dv.localhost" {
		add("*." + strings.TrimPrefix(baseHost, "*."))
	}

	return strings.Join(hosts, ", ")
}

// registerProxyRoutes returns errors; lifecycle callers report them as warnings.
func registerProxyRoutes(cfg config.Config, containerName, host string, containerPort int) error {
	containerIP, err := docker.ContainerIP(containerName)
	if err != nil {
		return fmt.Errorf("get proxy target for %s: %w", containerName, err)
	}
	target := fmt.Sprintf("http://%s:%d", containerIP, containerPort)
	var failures []error
	for _, routeHost := range append([]string{host}, cfg.HostnameAliases[containerName]...) {
		if err := localproxy.RegisterRoute(cfg.LocalProxy, routeHost, target); err != nil {
			failures = append(failures, fmt.Errorf("register %s: %w", routeHost, err))
		}
	}
	return errors.Join(failures...)
}

func registerContainerRoutes(cfg config.Config, name string) error {
	labels, err := labelsWithOverrides(name, cfg)
	if err != nil {
		return err
	}
	host, _, port, _, ok := localproxy.RouteFromLabels(labels)
	if !ok {
		return fmt.Errorf("container %s has no proxy routing metadata", name)
	}
	return registerProxyRoutes(cfg, name, host, port)
}

// Prefer the container's recorded primary, including rename overrides. A global
// suffix change must not silently replace an existing container's identity.
func containerPrimaryHostname(cfg config.Config, name string) (string, error) {
	labels, err := labelsWithOverrides(name, cfg)
	if err != nil {
		return "", fmt.Errorf("inspect primary hostname for %s: %w", name, err)
	}
	if host := strings.TrimSpace(labels[localproxy.LabelHost]); host != "" {
		return host, nil
	}
	return localproxy.HostnameForContainer(name, cfg.LocalProxy.Hostname), nil
}

func proxyExtraHosts(cfg config.Config, name, primary string) []string {
	if primary == "" {
		return nil
	}
	hosts := []string{primary + ":127.0.0.1"}
	for _, alias := range cfg.HostnameAliases[name] {
		hosts = append(hosts, alias+":127.0.0.1")
	}
	return hosts
}
