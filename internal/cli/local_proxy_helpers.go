package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

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
	envs["RAILS_DEVELOPMENT_HOSTS"] = host
	envs["DV_LOCAL_PROXY_HOST"] = host
	envs["DV_LOCAL_PROXY_HTTP_PORT"] = strconv.Itoa(lp.HTTPPort)
	if lp.HTTPS {
		envs["DV_LOCAL_PROXY_SCHEME"] = "https"
		envs["DV_LOCAL_PROXY_PORT"] = strconv.Itoa(lp.HTTPSPort)
		envs["DV_LOCAL_PROXY_HTTPS_PORT"] = strconv.Itoa(lp.HTTPSPort)
		envs["DISCOURSE_FORCE_HTTPS"] = "true"
		envs["DISCOURSE_DEV_ALLOW_HTTPS"] = "1"
		// Override DISCOURSE_PORT so URLs use the external HTTPS port, not the internal one
		envs["DISCOURSE_PORT"] = strconv.Itoa(lp.HTTPSPort)
	} else {
		envs["DV_LOCAL_PROXY_SCHEME"] = "http"
		envs["DV_LOCAL_PROXY_PORT"] = strconv.Itoa(lp.HTTPPort)
		// Override DISCOURSE_PORT so URLs use the external HTTP port, not the internal one
		envs["DISCOURSE_PORT"] = strconv.Itoa(lp.HTTPPort)
	}

	return host
}

func registerWithLocalProxy(cmd *cobra.Command, cfg config.Config, containerName string, host string, containerPort int) {
	if host == "" || containerPort <= 0 || !cfg.LocalProxy.Enabled {
		return
	}
	lp := cfg.LocalProxy
	lp.ApplyDefaults()
	if !localproxy.Running(lp) {
		return
	}
	// Get the container's internal IP address to route traffic directly
	containerIP, err := docker.ContainerIP(containerName)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Failed to get container IP for %s: %v\n", containerName, err)
		return
	}
	target := fmt.Sprintf("http://%s:%d", containerIP, containerPort)
	if err := localproxy.RegisterRoute(lp, host, target); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Failed to register %s at %s: %v\n", host, target, err)
	}
}

func registerContainerFromLabels(cmd *cobra.Command, cfg config.Config, name string) {
	if !cfg.LocalProxy.Enabled {
		return
	}
	labels, err := labelsWithOverrides(name, cfg)
	if err != nil {
		return
	}
	host, _, containerPort, _, ok := localproxy.RouteFromLabels(labels)
	if !ok {
		return
	}
	registerWithLocalProxy(cmd, cfg, name, host, containerPort)
}
