package cli

import (
	"fmt"
	"regexp"
	"strconv"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/localproxy"
)

var templateVarRe = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)

// interpolateVars replaces ${VAR} references in s for known vars only.
// Unknown variables and bare $VAR references are left unchanged.
func interpolateVars(s string, vars map[string]string) string {
	return templateVarRe.ReplaceAllStringFunc(s, func(match string) string {
		key := match[2 : len(match)-1]
		if val, ok := vars[key]; ok {
			return val
		}
		return match
	})
}

// buildTemplateVars computes the set of reserved variables available for
// interpolation in template env values. The container must be running.
func buildTemplateVars(cfg config.Config, name string) map[string]string {
	scheme, hostname, port := resolveDiscourseAccess(cfg, name)

	var discourseURL string
	if (scheme == "https" && port == 443) || (scheme == "http" && port == 80) {
		discourseURL = fmt.Sprintf("%s://%s", scheme, hostname)
	} else {
		discourseURL = fmt.Sprintf("%s://%s:%d", scheme, hostname, port)
	}

	return map[string]string{
		"DISCOURSE_HOSTNAME": hostname,
		"DISCOURSE_PORT":     strconv.Itoa(port),
		"DISCOURSE_SCHEME":   scheme,
		"DISCOURSE_URL":      discourseURL,
	}
}

// buildTemplateVarsFromEnvs derives the same four reserved variables from an
// already-populated envs map. Use this in sealProvisionedContainer where the
// proxy metadata has just been written into envs but no container is running
// yet to query.
func buildTemplateVarsFromEnvs(envs map[string]string) map[string]string {
	scheme := envs["DV_LOCAL_PROXY_SCHEME"]
	if scheme == "" {
		scheme = "http"
	}
	hostname := envs["DISCOURSE_HOSTNAME"]
	if hostname == "" {
		hostname = "localhost"
	}
	portStr := envs["DISCOURSE_PORT"]
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		port = 3000
	}

	var discourseURL string
	if (scheme == "https" && port == 443) || (scheme == "http" && port == 80) {
		discourseURL = fmt.Sprintf("%s://%s", scheme, hostname)
	} else {
		discourseURL = fmt.Sprintf("%s://%s:%d", scheme, hostname, port)
	}

	return map[string]string{
		"DISCOURSE_HOSTNAME": hostname,
		"DISCOURSE_PORT":     portStr,
		"DISCOURSE_SCHEME":   scheme,
		"DISCOURSE_URL":      discourseURL,
	}
}

func resolveDiscourseAccess(cfg config.Config, name string) (scheme, hostname string, port int) {
	lp := cfg.LocalProxy
	if lp.Enabled {
		hostname = localproxy.HostnameForContainer(name, lp.Hostname)
		if h, err := containerPrimaryHostname(cfg, name); err == nil && h != "" {
			hostname = h
		}
		if lp.HTTPS {
			scheme = "https"
			port = lp.HTTPSPort
		} else {
			scheme = "http"
			port = lp.HTTPPort
		}
		if lp.DiscoursePort > 0 {
			port = lp.DiscoursePort
		}
		return
	}

	// No local proxy — read port from container env, fall back to 3000.
	scheme = "http"
	hostname = "localhost"
	port = 3000
	if envMap, err := docker.GetContainerEnv(name); err == nil {
		if v, ok := envMap["DISCOURSE_PORT"]; ok {
			if p, err := strconv.Atoi(v); err == nil && p > 0 {
				port = p
			}
		}
	}
	return
}
