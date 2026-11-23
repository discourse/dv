package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"dv/internal/config"
)

func TestFrontendModeRuntimeScript(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
	localBin := filepath.Join(root, "usr-local-bin")
	workdir := filepath.Join(root, "discourse")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	run := filepath.Join(etc, "service", "ember", "run")
	write(run, "#!/bin/bash\necho watcher\n")
	write(filepath.Join(workdir, "frontend", "discourse", "rolldown.config.mjs"), "")
	if err := os.MkdirAll(filepath.Join(workdir, "log"), 0o755); err != nil {
		t.Fatal(err)
	}
	svLog := filepath.Join(root, "sv.log")
	bin := filepath.Join(root, "bin")
	write(filepath.Join(bin, "sv"), "#!/bin/sh\nif [ \"$1\" = status ]; then echo 'run: .: (pid 1)'; else echo \"$*\" >> "+shellQuote(svLog)+"; fi\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	apply := func(buildOnce bool) string {
		t.Helper()
		script := frontendModeRuntimeScript(buildOnce, workdir)
		script = strings.ReplaceAll(script, "/etc/", etc+"/")
		script = strings.ReplaceAll(script, "/usr/local/bin", localBin)
		// The real service idles after building; exit so the test can observe it.
		script = strings.ReplaceAll(script, "exec sleep infinity", "exit 0")
		cmd := exec.Command("sh", "-s")
		cmd.Stdin = strings.NewReader(script)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("script: %v\n%s", err, out)
		}
		// Stand in for pnpm so the service can be exercised without Discourse.
		write(filepath.Join(localBin, "dv-build-frontend"), "#!/bin/sh\necho built\n")
		return string(out)
	}
	runService := func() string {
		t.Helper()
		out, err := exec.Command("bash", run).CombinedOutput()
		if err != nil {
			t.Fatalf("service: %v\n%s", err, out)
		}
		return string(out)
	}

	if out := apply(true); !strings.Contains(out, "Restarting ember") {
		t.Fatalf("expected ember restart when enabling build-once mode: %q", out)
	}
	if out := apply(true); out != "" {
		t.Fatalf("idempotent apply restarted ember: %q", out)
	}
	if strings.Count(read(run), "# dv frontend mode") != 1 {
		t.Fatalf("expected one mode source line:\n%s", read(run))
	}
	if out := runService(); strings.Contains(out, "watcher") {
		t.Fatalf("build-once mode still started the watcher: %q", out)
	}
	if log := read(filepath.Join(workdir, "log", "ember.log")); !strings.Contains(log, "built") || !strings.Contains(log, "frontend build finished") {
		t.Fatalf("build-once mode did not build: %q", log)
	}

	// Legacy ember-cli layouts keep their watcher.
	if err := os.Remove(filepath.Join(workdir, "frontend", "discourse", "rolldown.config.mjs")); err != nil {
		t.Fatal(err)
	}
	if out := runService(); out != "watcher\n" {
		t.Fatalf("legacy layout should run the original service: %q", out)
	}
	write(filepath.Join(workdir, "frontend", "discourse", "rolldown.config.mjs"), "")

	if out := apply(false); !strings.Contains(out, "Restarting ember") {
		t.Fatalf("expected ember restart when re-enabling the watcher: %q", out)
	}
	if out := runService(); out != "watcher\n" {
		t.Fatalf("watch mode should run the original service: %q", out)
	}
	if got := strings.Count(read(svLog), "restart"); got != 2 {
		t.Fatalf("expected 2 restarts, got %d: %s", got, read(svLog))
	}
}

func TestFrontendModeRuntimeScriptSkipsImagesWithoutEmber(t *testing.T) {
	root := t.TempDir()
	script := strings.ReplaceAll(frontendModeRuntimeScript(true, "/var/www/discourse"), "/etc/", root+"/etc/")
	script = strings.ReplaceAll(script, "/usr/local/bin", filepath.Join(root, "bin"))
	cmd := exec.Command("sh", "-s")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("expected silent no-op without an ember service: %v %q", err, out)
	}
	if _, err := os.Stat(filepath.Join(root, "etc", "dv")); !os.IsNotExist(err) {
		t.Fatalf("expected no changes without an ember service: %v", err)
	}
}

func TestFrontendWatchConfigField(t *testing.T) {
	cfg := config.Default()
	if got, _ := getConfigField(cfg, "frontendWatch"); got != "true" {
		t.Fatalf("default frontendWatch = %q, want true", got)
	}
	if frontendWatchDisabled(cfg) {
		t.Fatal("watcher should be enabled by default")
	}
	if err := setConfigField(&cfg, "frontendWatch", "false"); err != nil {
		t.Fatal(err)
	}
	if got, _ := getConfigField(cfg, "frontendWatch"); got != "false" || !frontendWatchDisabled(cfg) {
		t.Fatalf("frontendWatch = %q after setting false", got)
	}
	if err := setConfigField(&cfg, "frontendWatch", "maybe"); err == nil {
		t.Fatal("expected an error for a non-boolean value")
	}
}
