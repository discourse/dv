package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dv/internal/config"
	"dv/internal/xdg"

	"github.com/spf13/cobra"
)

func TestNormalizeAlias(t *testing.T) {
	for _, input := range []string{"customers", "CUSTOMERS", "customers.dev.home.arpa", "customers.dev.home.arpa."} {
		got, err := normalizeAlias(input, "dev.home.arpa")
		if err != nil || got != "customers.dev.home.arpa" {
			t.Fatalf("%q: %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "-bad", "bad-", "bad_name", "*.forum", "site.forum.dev.home.arpa", "other.example.com", strings.Repeat("a", 64)} {
		if _, err := normalizeAlias(input, "dev.home.arpa"); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestAliasConflicts(t *testing.T) {
	cfg := config.Default()
	cfg.LocalProxy.Enabled = true
	cfg.LocalProxy.Hostname = "dev.home.arpa"
	cfg.ContainerImages["other_agent"] = "discourse"
	cfg.HostnameAliases = map[string][]string{"forum": {"customers.dev.home.arpa"}}
	for _, host := range []string{"other-agent.dev.home.arpa", "customers.dev.home.arpa", "forum.dev.home.arpa", "external.dev.home.arpa"} {
		if err := validateAliasOwner(cfg, "another", host, []string{"forum", "external"}); err == nil {
			t.Errorf("accepted conflicting %s", host)
		}
	}
	if err := validateAliasOwner(cfg, "forum", "customers.dev.home.arpa", nil); err != nil {
		t.Fatal(err)
	}
	if err := checkPrimaryHostname(cfg, "customers"); err == nil {
		t.Fatal("allowed primary to steal alias")
	}
}

func TestHostnameCommands(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bin := t.TempDir()
	script := `#!/bin/sh
case "$1" in
inspect) case "$*" in *forum*) echo '{"com.dv.local-proxy.host":"forum.dev.home.arpa"}';; *) exit 1;; esac;;
ps) case "$*" in *dv-local-proxy*) exit 0;; *) echo forum;; esac;;
exec) cat >/dev/null; exit 0;;
*) exit 1;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir, err := xdg.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.LocalProxy.Enabled = true
	cfg.LocalProxy.Hostname = "dev.home.arpa"
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	for _, action := range []string{"add", "add", "list"} {
		args := []string{"forum", "customers", "partners"}
		if action == "list" {
			args = args[:1]
		}
		if err := runHostname(cmd, action, args); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err = config.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.HostnameAliases["forum"]) != 2 {
		t.Fatalf("aliases: %v", cfg.HostnameAliases)
	}
	if !strings.Contains(out.String(), "forum.dev.home.arpa (primary)") {
		t.Fatal(out.String())
	}
	if err := runHostname(cmd, "add", []string{"forum", "valid", "bad_name"}); err == nil {
		t.Fatal("expected invalid alias")
	}
	cfg, _ = config.LoadOrCreate(dir)
	if len(cfg.HostnameAliases["forum"]) != 2 {
		t.Fatal("partial mutation")
	}
	if err := runHostname(cmd, "remove", []string{"forum", "customers", "partners"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = config.LoadOrCreate(dir)
	if aliases, managed := cfg.HostnameAliases["forum"]; !managed || len(aliases) != 0 {
		t.Fatalf("expected an empty managed entry for deferred cleanup: %v", cfg.HostnameAliases)
	}
}

func TestRenamePreservesHostnameAliases(t *testing.T) {
	setupRenameSelectionTest(t, "old-agent", "old-agent", "")
	dir, _ := xdg.ConfigDir()
	if err := config.Update(dir, func(cfg *config.Config) error {
		cfg.HostnameAliases = map[string][]string{"old-agent": {"customers.dv.localhost"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	runRenameWithDockerStub(t, "old-agent", "new-agent")
	cfg, err := config.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.HostnameAliases["old-agent"]) != 0 || len(cfg.HostnameAliases["new-agent"]) != 1 {
		t.Fatalf("aliases: %v", cfg.HostnameAliases)
	}
}

func TestAliasApplicationMetadata(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := config.Default()
	cfg.LocalProxy.Enabled = true
	cfg.LocalProxy.Hostname = "dev.home.arpa"
	cfg.HostnameAliases = map[string][]string{"forum": {"customers.dev.home.arpa", "partners.dev.home.arpa"}}
	envs := map[string]string{}
	host := applyLocalProxyMetadata(cfg, "forum", 3001, 3000, map[string]string{}, envs)
	if host != "forum.dev.home.arpa" || envs["DISCOURSE_HOSTNAME"] != host {
		t.Fatalf("primary changed: %v", envs)
	}
	if envs["RAILS_DEVELOPMENT_HOSTS"] != "forum.dev.home.arpa,customers.dev.home.arpa,partners.dev.home.arpa" {
		t.Fatalf("Rails hosts: %v", envs)
	}
	for _, alias := range cfg.HostnameAliases["forum"] {
		if !strings.Contains(envs["DV_CADDY_HOSTS"], alias) {
			t.Fatalf("Caddy missing %s", alias)
		}
	}
}

func TestHostnameProxyUpgradeNotice(t *testing.T) {
	for _, tc := range []struct {
		name, script, want string
		https, disabled    bool
	}{
		{name: "old proxy", script: `echo '["PROXY_HTTP_ADDR=:80"]'`, want: "Your proxy needs an upgrade to support hostname aliases. Run: dv config local-proxy --rebuild --recreate\n"},
		{name: "old HTTPS proxy", script: `echo '[]'`, https: true, want: "Your proxy needs an upgrade to support hostname aliases. Run: dv config local-proxy --rebuild --recreate\n"},
		{name: "upgraded proxy", script: `echo '["PROXY_ALIASES_FILE=/etc/local-proxy/aliases/aliases.json"]'`},
		{name: "missing proxy or inspection failure", script: "exit 1"},
		{name: "invalid inspection response", script: "echo invalid"},
		{name: "disabled proxy", script: `echo '[]'`, disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			cfg := config.Default().LocalProxy
			cfg.Enabled = !tc.disabled
			cfg.HTTPS = tc.https
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetErr(&out)
			warnHostnameProxyUpgrade(cmd, cfg)
			if got := out.String(); got != tc.want {
				t.Fatalf("notice = %q, want %q", got, tc.want)
			}
		})
	}
}
