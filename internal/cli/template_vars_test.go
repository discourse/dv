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
