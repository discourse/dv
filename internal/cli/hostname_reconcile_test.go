package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dv/internal/config"
	"dv/internal/xdg"

	"github.com/spf13/cobra"
)

func setupHostnameReconcileTest(t *testing.T) (*cobra.Command, string, string, *atomic.Bool) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("DV_AGENT", "")
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "unavailable", 503)
			return
		}
		if r.Method == "DELETE" {
			w.WriteHeader(204)
		} else {
			w.WriteHeader(201)
		}
	}))
	t.Cleanup(server.Close)
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "scripts")
	t.Setenv("DV_TEST_SCRIPT_LOG", log)
	t.Setenv("DV_TEST_PRIMARY", "forum.dev.home.arpa")
	script := `#!/bin/sh
case "$1" in
ps) echo forum;;
inspect) case "$*" in
 *Config.Labels*) printf '{"com.dv.local-proxy.host":"%s","com.dv.local-proxy.target-port":"3001","com.dv.local-proxy.container-port":"3000"}\n' "$DV_TEST_PRIMARY";;
 *NetworkSettings*) echo 127.0.0.2;;
 *Config.Env*) echo '["PROXY_ALIASES_FILE=/etc/local-proxy/aliases/aliases.json"]';;
 *) exit 1;;
 esac;;
rm) exit 0;;
exec) cat >> "$DV_TEST_SCRIPT_LOG"; if [ "${DV_TEST_FAIL_EXEC:-}" = 1 ]; then echo 'service unavailable' >&2; exit 1; fi;;
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
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	cfg.LocalProxy.APIPort, _ = strconv.Atoi(port)
	cfg.ContainerImages["forum"] = "discourse"
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return cmd, dir, log, &fail
}

func TestHostnamePartialFailureCanBeRetried(t *testing.T) {
	cmd, dir, log, fail := setupHostnameReconcileTest(t)
	fail.Store(true)
	if err := runHostname(cmd, "add", []string{"forum", "customers"}); err == nil || !strings.Contains(err.Error(), "not fully applied") {
		t.Fatalf("add: %v", err)
	}
	if _, err := os.Stat(log); err != nil {
		t.Fatal("application sync was skipped after proxy failure")
	}
	fail.Store(false)
	if err := runHostname(cmd, "add", []string{"forum", "customers"}); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if err := runHostname(cmd, "remove", []string{"forum", "customers"}); err == nil {
		t.Fatal("reported success on failed live removal")
	}
	cfg, _ := config.LoadOrCreate(dir)
	if len(cfg.HostnameAliases["forum"]) != 0 || len(cfg.HostnameRemovals["forum"]) != 1 {
		t.Fatalf("missing cleanup journal: %+v", cfg.HostnameRemovals)
	}
	if err := checkPrimaryHostname(cfg, "customers"); err == nil {
		t.Fatal("allowed primary to steal pending cleanup route")
	}
	fail.Store(false)
	if err := runHostname(cmd, "remove", []string{"forum", "customers"}); err != nil {
		t.Fatal(err)
	}
	if err := runHostname(cmd, "remove", []string{"forum", "customers"}); err != nil {
		t.Fatalf("remove not idempotent: %v", err)
	}
	cfg, _ = config.LoadOrCreate(dir)
	if len(cfg.HostnameRemovals) != 0 {
		t.Fatalf("cleanup remains: %+v", cfg.HostnameRemovals)
	}
}

func TestHostnamePrimaryUsesRecordedIdentity(t *testing.T) {
	cmd, _, log, _ := setupHostnameReconcileTest(t)
	t.Setenv("DV_TEST_PRIMARY", "forum.old.home.arpa")
	if err := runHostname(cmd, "add", []string{"forum", "customers"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "forum.old.home.arpa,customers.dev.home.arpa") {
		t.Fatalf("primary identity changed: %s", data)
	}
}

func TestHostnameLifecycleFailureWarnsWithoutBlocking(t *testing.T) {
	cmd, dir, _, _ := setupHostnameReconcileTest(t)
	cfg, _ := config.LoadOrCreate(dir)
	cfg.HostnameAliases = map[string][]string{"forum": {"customers.dev.home.arpa"}}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DV_TEST_FAIL_EXEC", "1")
	var output bytes.Buffer
	cmd.SetErr(&output)
	syncContainerHostnamesBestEffort(cmd, cfg, "forum")
	if !strings.Contains(output.String(), "Warning:") || !strings.Contains(output.String(), "service unavailable") {
		t.Fatal(output.String())
	}
	cfg.LocalProxy.Enabled = false
	if err := checkPrimaryHostname(cfg, "customers"); err != nil {
		t.Fatal("disabled proxy blocked start", err)
	}
}

func TestRenameSynchronizesManagedHostnameEnvironment(t *testing.T) {
	cmd, dir, log, _ := setupHostnameReconcileTest(t)
	if err := config.Update(dir, func(cfg *config.Config) error {
		cfg.HostnameAliases = map[string][]string{"forum": {"customers.dev.home.arpa"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	oldExists, oldRename := renameDockerExists, renameDockerRenameContext
	renameDockerExists = func(name string) bool { return name == "forum" }
	renameDockerRenameContext = func(context.Context, string, string, io.Writer, io.Writer) error { return nil }
	t.Cleanup(func() { renameDockerExists, renameDockerRenameContext = oldExists, oldRename })
	if err := runRename(cmd, "forum", "shop"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "shop.dev.home.arpa,customers.dev.home.arpa") {
		t.Fatalf("renamed primary missing from runtime settings: %s", data)
	}
}

func TestHostnameCleanupStaleSnapshotDoesNotPanic(t *testing.T) {
	_, dir, _, _ := setupHostnameReconcileTest(t)
	if err := config.Update(dir, func(cfg *config.Config) error {
		queueHostnameRemovals(cfg, "forum", "customers.dev.home.arpa")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stale, _ := config.LoadOrCreate(dir)
	for i := 0; i < 2; i++ {
		if err := cleanupHostnameRoutes(dir, stale, "forum"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHostnameReAddCancelsPendingRemoval(t *testing.T) {
	cmd, dir, _, fail := setupHostnameReconcileTest(t)
	if err := runHostname(cmd, "add", []string{"forum", "customers"}); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if err := runHostname(cmd, "remove", []string{"forum", "customers"}); err == nil {
		t.Fatal("expected pending removal")
	}
	fail.Store(false)
	if err := runHostname(cmd, "add", []string{"forum", "customers"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.LoadOrCreate(dir)
	if len(cfg.HostnameRemovals["forum"]) != 0 || len(cfg.HostnameAliases["forum"]) != 1 {
		t.Fatal("re-add retained removal intent")
	}
}

func TestHostnameCleanupAfterExternalContainerRemoval(t *testing.T) {
	cmd, dir, _, _ := setupHostnameReconcileTest(t)
	if err := config.Update(dir, func(cfg *config.Config) error { queueHostnameRemovals(cfg, "gone", "gone.dev.home.arpa"); return nil }); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	stub := "#!/bin/sh\ncase \"$1\" in inspect) exit 1;; ps) case \"$*\" in *dv-local-proxy*) echo proxy;; esac;; *) exit 1;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for i := 0; i < 2; i++ {
		if err := runHostname(cmd, "remove", []string{"gone", "gone"}); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _ := config.LoadOrCreate(dir)
	if len(cfg.HostnameRemovals["gone"]) != 0 {
		t.Fatal("retired primary cleanup pending")
	}
}

func TestNilCommandHostnameSyncForCustomContainer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := config.Default()
	cfg.LocalProxy.Enabled = true
	cfg.Images["custom"] = config.ImageConfig{Kind: "custom"}
	cfg.ContainerImages["forum"] = "custom"
	cfg.HostnameAliases = map[string][]string{"forum": {}}
	dir, err := xdg.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := syncContainerHostnames(nil, cfg, "forum"); err != nil {
		t.Fatal(err)
	}
	syncContainerHostnamesBestEffort(nil, cfg, "forum")
}

func TestProxyExtraHostsIncludesAliases(t *testing.T) {
	cfg := config.Default()
	cfg.HostnameAliases = map[string][]string{"forum": {"customers.dev.home.arpa"}}
	got := proxyExtraHosts(cfg, "forum", "forum.dev.home.arpa")
	if strings.Join(got, ",") != "forum.dev.home.arpa:127.0.0.1,customers.dev.home.arpa:127.0.0.1" {
		t.Fatal(got)
	}
	if got := proxyExtraHosts(cfg, "forum", ""); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestHostnameOperationLockHonorsCancellation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() { done <- withHostnameOperationLock(nil, func() error { close(entered); <-release; return nil }) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	err := withHostnameOperationLock(cmd, func() error { return fmt.Errorf("unexpected lock acquisition") })
	close(release)
	if first := <-done; first != nil {
		t.Fatal(first)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock error: %v", err)
	}
}

func TestLifecycleReloadsNewAliasAfterStartupSnapshot(t *testing.T) {
	cmd, dir, log, _ := setupHostnameReconcileTest(t)
	stale, _ := config.LoadOrCreate(dir)
	if err := config.Update(dir, func(cfg *config.Config) error {
		cfg.HostnameAliases = map[string][]string{"forum": {"customers.dev.home.arpa"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	syncContainerHostnamesBestEffort(cmd, stale, "forum")
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "customers.dev.home.arpa") {
		t.Fatal("new aliases skipped by stale startup snapshot")
	}
}

func TestServeRenameUsesHostnameLifecycle(t *testing.T) {
	_, dir, log, _ := setupHostnameReconcileTest(t)
	if err := config.Update(dir, func(cfg *config.Config) error {
		cfg.HostnameAliases = map[string][]string{"forum": {"customers.dev.home.arpa"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	oldExists, oldRename := renameDockerExists, renameDockerRenameContext
	renameDockerExists = func(name string) bool { return name == "forum" }
	renameDockerRenameContext = func(context.Context, string, string, io.Writer, io.Writer) error { return nil }
	t.Cleanup(func() { renameDockerExists, renameDockerRenameContext = oldExists, oldRename })
	out := httptest.NewRecorder()
	handleContainerRename(out, httptest.NewRequest("POST", "/", strings.NewReader(`{"new_name":"shop"}`)), dir, "forum")
	if out.Code != 200 {
		t.Fatalf("rename response %d: %s", out.Code, out.Body.String())
	}
	cfg, _ := config.LoadOrCreate(dir)
	if len(cfg.HostnameAliases["shop"]) != 1 || len(cfg.HostnameAliases["forum"]) != 0 {
		t.Fatal("web rename lost aliases")
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "shop.dev.home.arpa,customers.dev.home.arpa") {
		t.Fatal("web rename skipped runtime sync")
	}
}

func TestServeDeleteUsesHostnameLifecycle(t *testing.T) {
	_, dir, _, _ := setupHostnameReconcileTest(t)
	if err := config.Update(dir, func(cfg *config.Config) error {
		cfg.HostnameAliases = map[string][]string{"forum": {"customers.dev.home.arpa"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	restore := stubRemoveDocker(t)
	defer restore()
	out := httptest.NewRecorder()
	handleContainerDelete(out, httptest.NewRequest("POST", "/", strings.NewReader(`{"force":true}`)), dir, "forum")
	if out.Code != 200 {
		t.Fatalf("delete response %d: %s", out.Code, out.Body.String())
	}
	cfg, _ := config.LoadOrCreate(dir)
	if _, ok := cfg.HostnameAliases["forum"]; ok {
		t.Fatal("web delete left alias owner")
	}
	if len(cfg.HostnameRemovals["forum"]) != 0 {
		t.Fatal("web delete did not clean live routes")
	}
}
