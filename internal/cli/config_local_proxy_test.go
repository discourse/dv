package cli

import (
	"dv/internal/config"
	"dv/internal/xdg"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestLocalProxyHTTPSFlag(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current bool
		args    []string
		want    bool
	}{
		{name: "new proxy defaults to HTTP"},
		{name: "preserves HTTPS", current: true, want: true},
		{name: "rebuild preserves HTTPS", current: true, args: []string{"--rebuild", "--recreate"}, want: true},
		{name: "rebuild preserves HTTP", args: []string{"--rebuild", "--recreate"}},
		{name: "explicit enable", args: []string{"--https"}, want: true},
		{name: "explicit disable", current: true, args: []string{"--https=false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.Flags().Bool("https", false, "")
			cmd.Flags().Bool("rebuild", false, "")
			cmd.Flags().Bool("recreate", false, "")
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			if got := localProxyHTTPSFlag(cmd, tc.current); got != tc.want {
				t.Fatalf("HTTPS=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestLocalProxySuffixChangeRejectsPendingRemovals(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := xdg.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.LocalProxy.Hostname = "dev.home.arpa"
	cfg.HostnameRemovals = map[string][]string{"forum": {"customers.dev.home.arpa"}}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.Flags().String("hostname", "", "")
	if err := cmd.Flags().Set("hostname", "other.home.arpa"); err != nil {
		t.Fatal(err)
	}
	if err := configLocalProxyCmd.RunE(cmd, nil); err == nil || !strings.Contains(err.Error(), "pending hostname removals") {
		t.Fatalf("error: %v", err)
	}
}
