package cli

import (
	"testing"

	"dv/internal/config"
)

func TestInterpolateVars(t *testing.T) {
	vars := map[string]string{
		"DISCOURSE_URL":      "https://mycontainer.dv.localhost",
		"DISCOURSE_HOSTNAME": "mycontainer.dv.localhost",
		"DISCOURSE_PORT":     "443",
		"DISCOURSE_SCHEME":   "https",
	}

	cases := []struct {
		in   string
		want string
	}{
		{"${DISCOURSE_URL}/admin", "https://mycontainer.dv.localhost/admin"},
		{"${DISCOURSE_SCHEME}://${DISCOURSE_HOSTNAME}", "https://mycontainer.dv.localhost"},
		{"${UNKNOWN_VAR}", "${UNKNOWN_VAR}"},
		{"$DISCOURSE_URL", "$DISCOURSE_URL"},
		{"no vars here", "no vars here"},
		{"port=${DISCOURSE_PORT}", "port=443"},
	}

	for _, c := range cases {
		got := interpolateVars(c.in, vars)
		if got != c.want {
			t.Errorf("interpolateVars(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveDiscourseAccess(t *testing.T) {
	t.Run("local proxy http", func(t *testing.T) {
		cfg := config.Config{}
		cfg.LocalProxy = config.LocalProxyConfig{
			Enabled:  true,
			Hostname: "dv.localhost",
			HTTPPort: 80,
		}
		scheme, hostname, port := resolveDiscourseAccess(cfg, "mycontainer")
		if scheme != "http" {
			t.Errorf("scheme = %q, want %q", scheme, "http")
		}
		if hostname != "mycontainer.dv.localhost" {
			t.Errorf("hostname = %q, want %q", hostname, "mycontainer.dv.localhost")
		}
		if port != 80 {
			t.Errorf("port = %d, want 80", port)
		}
	})

	t.Run("local proxy https", func(t *testing.T) {
		cfg := config.Config{}
		cfg.LocalProxy = config.LocalProxyConfig{
			Enabled:   true,
			Hostname:  "dv.localhost",
			HTTPS:     true,
			HTTPSPort: 443,
		}
		scheme, hostname, port := resolveDiscourseAccess(cfg, "mycontainer")
		if scheme != "https" {
			t.Errorf("scheme = %q, want %q", scheme, "https")
		}
		if port != 443 {
			t.Errorf("port = %d, want 443", port)
		}
		_ = hostname
	})

	t.Run("discourse_port override", func(t *testing.T) {
		cfg := config.Config{}
		cfg.LocalProxy = config.LocalProxyConfig{
			Enabled:       true,
			Hostname:      "dv.localhost",
			HTTPPort:      80,
			DiscoursePort: 8080,
		}
		_, _, port := resolveDiscourseAccess(cfg, "mycontainer")
		if port != 8080 {
			t.Errorf("port = %d, want 8080", port)
		}
	})

	t.Run("no local proxy falls back to localhost:3000", func(t *testing.T) {
		cfg := config.Config{}
		scheme, hostname, port := resolveDiscourseAccess(cfg, "mycontainer")
		if scheme != "http" {
			t.Errorf("scheme = %q, want %q", scheme, "http")
		}
		if hostname != "localhost" {
			t.Errorf("hostname = %q, want %q", hostname, "localhost")
		}
		if port != 3000 {
			t.Errorf("port = %d, want 3000", port)
		}
	})
}

func TestBuildTemplateVarsFromEnvs(t *testing.T) {
	cases := []struct {
		name       string
		envs       map[string]string
		wantURL    string
		wantScheme string
		wantHost   string
		wantPort   string
	}{
		{
			name: "proxy http port 80 omits port from URL",
			envs: map[string]string{
				"DISCOURSE_HOSTNAME":    "mysite.dv.localhost",
				"DISCOURSE_PORT":        "80",
				"DV_LOCAL_PROXY_SCHEME": "http",
			},
			wantURL:    "http://mysite.dv.localhost",
			wantScheme: "http",
			wantHost:   "mysite.dv.localhost",
			wantPort:   "80",
		},
		{
			name: "proxy https port 443 omits port from URL",
			envs: map[string]string{
				"DISCOURSE_HOSTNAME":    "mysite.dv.localhost",
				"DISCOURSE_PORT":        "443",
				"DV_LOCAL_PROXY_SCHEME": "https",
			},
			wantURL:    "https://mysite.dv.localhost",
			wantScheme: "https",
			wantHost:   "mysite.dv.localhost",
			wantPort:   "443",
		},
		{
			name: "non-standard port included in URL",
			envs: map[string]string{
				"DISCOURSE_HOSTNAME":    "mysite.dv.localhost",
				"DISCOURSE_PORT":        "9292",
				"DV_LOCAL_PROXY_SCHEME": "http",
			},
			wantURL:    "http://mysite.dv.localhost:9292",
			wantScheme: "http",
			wantHost:   "mysite.dv.localhost",
			wantPort:   "9292",
		},
		{
			name:       "no proxy falls back to localhost:3000",
			envs:       map[string]string{},
			wantURL:    "http://localhost:3000",
			wantScheme: "http",
			wantHost:   "localhost",
			wantPort:   "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vars := buildTemplateVarsFromEnvs(c.envs)
			if got := vars["DISCOURSE_URL"]; got != c.wantURL {
				t.Errorf("DISCOURSE_URL = %q, want %q", got, c.wantURL)
			}
			if got := vars["DISCOURSE_SCHEME"]; got != c.wantScheme {
				t.Errorf("DISCOURSE_SCHEME = %q, want %q", got, c.wantScheme)
			}
			if got := vars["DISCOURSE_HOSTNAME"]; got != c.wantHost {
				t.Errorf("DISCOURSE_HOSTNAME = %q, want %q", got, c.wantHost)
			}
			if c.wantPort != "" {
				if got := vars["DISCOURSE_PORT"]; got != c.wantPort {
					t.Errorf("DISCOURSE_PORT = %q, want %q", got, c.wantPort)
				}
			}
		})
	}
}

// TestSealEnvInterpolation covers the regression where template env values
// containing ${DISCOURSE_URL} were passed raw to docker run because
// interpolation only happened in executeTemplate (docker exec), not in
// sealProvisionedContainer or ensureContainerRunningWithWorkdirResult (docker run).
func TestSealEnvInterpolation(t *testing.T) {
	// Simulate envs after applyLocalProxyMetadata has run with a proxy active.
	envs := map[string]string{
		"DISCOURSE_HOSTNAME":    "mysite.dv.localhost",
		"DISCOURSE_PORT":        "443",
		"DV_LOCAL_PROXY_SCHEME": "https",
	}

	templateEnvs := map[string]string{
		"DISCOURSE_PLUGIN_CALLBACK_URL":      "${DISCOURSE_URL}/auth/callback",
		"DISCOURSE_PLUGIN_ORCHESTRATION_URL": "${DISCOURSE_URL}/auth",
		"DISCOURSE_PLUGIN_TOKEN_URL":         "http://127.0.0.1:3000/auth/token",
		"DISCOURSE_PLUGIN_API_URL":           "http://127.0.0.1:3000",
	}
	for k, v := range templateEnvs {
		envs[k] = v
	}

	tvars := buildTemplateVarsFromEnvs(envs)
	for k := range templateEnvs {
		if v, ok := envs[k]; ok {
			envs[k] = interpolateVars(v, tvars)
		}
	}

	want := map[string]string{
		"DISCOURSE_PLUGIN_CALLBACK_URL":      "https://mysite.dv.localhost/auth/callback",
		"DISCOURSE_PLUGIN_ORCHESTRATION_URL": "https://mysite.dv.localhost/auth",
		"DISCOURSE_PLUGIN_TOKEN_URL":         "http://127.0.0.1:3000/auth/token",
		"DISCOURSE_PLUGIN_API_URL":           "http://127.0.0.1:3000",
	}
	for k, wantVal := range want {
		if got := envs[k]; got != wantVal {
			t.Errorf("%s = %q, want %q", k, got, wantVal)
		}
	}
}

func TestBuildTemplateVars_URL(t *testing.T) {
	cases := []struct {
		name    string
		cfg     config.Config
		wantURL string
	}{
		{
			name: "http port 80 omits port",
			cfg: config.Config{LocalProxy: config.LocalProxyConfig{
				Enabled: true, Hostname: "dv.localhost", HTTPPort: 80,
			}},
			wantURL: "http://mycontainer.dv.localhost",
		},
		{
			name: "https port 443 omits port",
			cfg: config.Config{LocalProxy: config.LocalProxyConfig{
				Enabled: true, Hostname: "dv.localhost", HTTPS: true, HTTPSPort: 443,
			}},
			wantURL: "https://mycontainer.dv.localhost",
		},
		{
			name: "non-standard port included",
			cfg: config.Config{LocalProxy: config.LocalProxyConfig{
				Enabled: true, Hostname: "dv.localhost", HTTPPort: 3000,
			}},
			wantURL: "http://mycontainer.dv.localhost:3000",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vars := buildTemplateVars(c.cfg, "mycontainer")
			if got := vars["DISCOURSE_URL"]; got != c.wantURL {
				t.Errorf("DISCOURSE_URL = %q, want %q", got, c.wantURL)
			}
		})
	}
}
