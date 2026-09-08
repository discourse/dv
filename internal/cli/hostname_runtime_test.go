package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostnameRuntimeScript(t *testing.T) {
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
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
	write(filepath.Join(etc, "hosts"), "127.0.0.1 localhost\n127.0.0.1 forum.dev.home.arpa keep-me\n10.0.0.1 untouched\n# preserve comment\n")
	for _, svc := range []string{"rails", "caddy"} {
		write(filepath.Join(etc, "service", svc, "run"), "#!/bin/sh\nprintf '%s' \"$RAILS_DEVELOPMENT_HOSTS\"\n")
	}
	write(filepath.Join(etc, "service", "caddy", "finish"), "#!/bin/sh\npkill -f \"caddy\" || true\n# preserve custom cleanup\n")
	log := filepath.Join(root, "sv.log")
	bin := filepath.Join(root, "bin")
	write(filepath.Join(bin, "sv"), "#!/bin/sh\ncase \"$* $(ps -o args= -p \"$PPID\")\" in *caddy*) echo 'would be killed by pkill -f caddy' >&2; exit 143;; esac\nif [ \"$1\" = status ]; then echo 'run: service: (pid 1)'; else echo \"$*\" >> "+shellQuote(log)+"; fi\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	apply := func(hosts string) string {
		t.Helper()
		env := "export RAILS_DEVELOPMENT_HOSTS=" + shellQuote(strings.ReplaceAll(hosts, " ", ",")) + "\nexport DV_CADDY_HOSTS=" + shellQuote(strings.ReplaceAll(hosts, " ", ", ")) + "\n"
		script := strings.ReplaceAll(hostnameRuntimeScript(env, hosts), "/etc/", etc+"/")
		cmd := exec.Command("sh", "-s")
		cmd.Stdin = strings.NewReader(script)
		cmd.Env = append(os.Environ(), "RAILS_DEVELOPMENT_HOSTS=forum.dev.home.arpa")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("script: %v\n%s", err, out)
		}
		return string(out)
	}
	hosts := "forum.dev.home.arpa customers.dev.home.arpa"
	if out := apply(hosts); strings.Count(out, "Restarting") != 2 {
		t.Fatal(out)
	}
	finish := read(filepath.Join(etc, "service", "caddy", "finish"))
	if finish != "#!/bin/sh\npkill -x caddy || true\n# preserve custom cleanup\n" {
		t.Fatalf("unsafe cleanup hook or custom content lost: %s", finish)
	}
	if out := apply(hosts); out != "" {
		t.Fatalf("idempotent apply restarted services: %s", out)
	}
	hostFile := read(filepath.Join(etc, "hosts"))
	for _, want := range []string{"localhost", "keep-me", "10.0.0.1 untouched", "# preserve comment", "127.0.0.1 " + hosts} {
		if !strings.Contains(hostFile, want) {
			t.Fatalf("hosts missing %q: %s", want, hostFile)
		}
	}
	for _, svc := range []string{"rails", "caddy"} {
		run := filepath.Join(etc, "service", svc, "run")
		if strings.Count(read(run), "# dv hostname aliases") != 1 {
			t.Fatal("duplicate launcher source")
		}
		out, err := exec.Command("sh", run).CombinedOutput()
		if err != nil || string(out) != "forum.dev.home.arpa,customers.dev.home.arpa" {
			t.Fatalf("persistent launcher: %s, %v", out, err)
		}
	}
	apply("forum.dev.home.arpa")
	if strings.Contains(read(filepath.Join(etc, "hosts")), "customers") {
		t.Fatal("removed alias remains in hosts")
	}
	if strings.Contains(read(filepath.Join(etc, "dv", "hostname-env")), "customers") {
		t.Fatal("removed alias remains in environment")
	}
	if got := strings.Count(read(log), "restart"); got != 4 {
		t.Fatalf("restart count=%d", got)
	}
	write(filepath.Join(bin, "sv"), "#!/bin/sh\nif [ \"$1\" = status ]; then echo 'down: service: 0s'; else echo unexpected-start >> "+shellQuote(log)+"; fi\n")
	if out := apply(hosts); strings.Contains(out, "Restarting") {
		t.Fatalf("started a stopped service: %s", out)
	}
	if strings.Contains(read(log), "unexpected-start") {
		t.Fatal("stopped service was started")
	}
}
