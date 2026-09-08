package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxyAliasProjection(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.Env = map[string]string{"SECRET": "do-not-export"}
	cfg.HostnameAliases = map[string][]string{"forum": {"customers.dev.home.arpa"}}
	if err := Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "proxy-aliases", "aliases.json"))
		if err != nil {
			t.Fatal(err)
		}
		var owners map[string]string
		if err := json.Unmarshal(data, &owners); err != nil {
			t.Fatal(err)
		}
		return owners
	}
	owners := read()
	if len(owners) != 1 || owners["customers.dev.home.arpa"] != "forum" {
		t.Fatalf("projection: %v", owners)
	}
	if err := Update(dir, func(c *Config) error { delete(c.HostnameAliases, "forum"); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(read()) != 0 {
		t.Fatal("stale alias projection")
	}
	loaded, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Env["SECRET"] != "do-not-export" {
		t.Fatal("lost secret")
	}
}

func TestProxyProjectionFailureIsReconciledWithoutRewritingConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.HostnameAliases = map[string][]string{"forum": {"customers.dev.home.arpa"}}
	projectionDir := filepath.Join(dir, "proxy-aliases")
	if err := os.WriteFile(projectionDir, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, cfg); err == nil || !strings.Contains(err.Error(), "configuration saved") {
		t.Fatalf("error: %v", err)
	}
	before, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(projectionDir); err != nil {
		t.Fatal(err)
	}
	if err := PublishProxyAliases(dir); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("publishing projection rewrote config")
	}
	for _, tc := range []struct {
		path string
		mode os.FileMode
	}{{Path(dir), 0o600}, {filepath.Join(projectionDir, "aliases.json"), 0o644}} {
		info, err := os.Stat(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != tc.mode {
			t.Fatalf("%s mode %v", tc.path, info.Mode())
		}
	}
}

func TestStaleSettingsSavePreservesRoutingState(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Default()); err != nil {
		t.Fatal(err)
	}
	stale, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Update(dir, func(cfg *Config) error {
		cfg.HostnameAliases = map[string][]string{"forum": {"customers.dev.home.arpa"}}
		cfg.HostnameRemovals = map[string][]string{"gone": {"gone.dev.home.arpa"}}
		cfg.LabelOverrides = map[string]map[string]string{"forum": {"com.dv.local-proxy.host": "forum.dev.home.arpa"}}
		cfg.LocalProxy.HTTPS = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stale.SelectedAgent = "other"
	if err := Save(dir, stale); err != nil {
		t.Fatal(err)
	}
	latest, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.HostnameAliases["forum"]) != 1 || len(latest.HostnameRemovals["gone"]) != 1 || !latest.LocalProxy.HTTPS || len(latest.LabelOverrides["forum"]) != 1 || latest.SelectedAgent != "other" {
		t.Fatalf("stale save reverted routing state: %+v", latest)
	}
	latest.HostnameAliases["forum"] = nil
	if err := Save(dir, latest); err != nil {
		t.Fatal(err)
	}
	latest, _ = LoadOrCreate(dir)
	if len(latest.HostnameAliases["forum"]) != 0 {
		t.Fatal("intentional hostname edit was discarded")
	}
}

func TestUnrelatedConfigUpdateSurvivesProjectionFailure(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Default()); err != nil {
		t.Fatal(err)
	}
	projectionDir := filepath.Join(dir, "proxy-aliases")
	if err := os.RemoveAll(projectionDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectionDir, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Update(dir, func(cfg *Config) error { cfg.SelectedAgent = "other"; return nil }); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadOrCreate(dir)
	cfg.Workdir = "/another"
	if err := Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	err := Update(dir, func(cfg *Config) error {
		cfg.HostnameAliases = map[string][]string{"forum": {"customers.dev.home.arpa"}}
		return nil
	})
	if !IsProjectionError(err) {
		t.Fatalf("expected committed projection error: %v", err)
	}
}

func TestInvalidAliasOwnershipRejectedBeforeConfigCommit(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Default()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(Path(dir))
	cfg, _ := LoadOrCreate(dir)
	cfg.HostnameAliases = map[string][]string{"one": {"same.dev.home.arpa"}, "two": {"same.dev.home.arpa"}}
	if err := Save(dir, cfg); err == nil || IsProjectionError(err) {
		t.Fatalf("expected validation error before commit: %v", err)
	}
	after, _ := os.ReadFile(Path(dir))
	if string(after) != string(before) {
		t.Fatal("invalid alias ownership committed")
	}
}
