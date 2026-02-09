package cli

import (
	"strings"
	"testing"
)

func TestBuildCatchupScript_CoreOnly(t *testing.T) {
	t.Parallel()

	script := buildCatchupScript("/var/www/discourse", nil)

	// Should have pipefail
	if !strings.Contains(script, "set -euo pipefail") {
		t.Error("missing set -euo pipefail")
	}

	// Should stop and restart services
	if !strings.Contains(script, "force-stop unicorn") {
		t.Error("missing service stop")
	}
	if !strings.Contains(script, "trap cleanup EXIT") {
		t.Error("missing cleanup trap")
	}

	// Should reset and fetch+reset core
	if !strings.Contains(script, "git reset --hard\ngit clean -df") {
		t.Error("missing git reset --hard + git clean -df for core")
	}
	if !strings.Contains(script, "git fetch --prune\ngit reset --hard @{u}") {
		t.Error("missing git fetch --prune + git reset --hard @{u} for core")
	}

	// Should NOT contain git pull (we use fetch+reset instead)
	if strings.Contains(script, "git pull") {
		t.Error("should not use git pull; use git fetch + git reset --hard @{u}")
	}

	// Should install deps
	if !strings.Contains(script, "bundle install") {
		t.Error("missing bundle install")
	}
	if !strings.Contains(script, "pnpm install") {
		t.Error("missing pnpm install")
	}

	// Should migrate both databases
	if !strings.Contains(script, "bin/rake db:migrate") {
		t.Error("missing dev db:migrate")
	}
	if !strings.Contains(script, "RAILS_ENV=test bin/rake db:migrate") {
		t.Error("missing test db:migrate")
	}
}

func TestBuildCatchupScript_WithPlugins(t *testing.T) {
	t.Parallel()

	plugins := []string{"plugins/discourse-ai", "plugins/discourse-automation"}
	script := buildCatchupScript("/var/www/discourse", plugins)

	// Each plugin should have cd, reset, fetch+reset, and cd back
	for _, p := range plugins {
		if !strings.Contains(script, "cd "+shellQuote(p)) {
			t.Errorf("missing cd into %s", p)
		}
		if !strings.Contains(script, "cd "+shellQuote("/var/www/discourse")) {
			t.Errorf("missing cd back to workdir after %s", p)
		}
	}

	// Plugin echo messages should be present (shellQuote for simple strings
	// produces the same 'string' format, but the important case is names with
	// single quotes — tested in TestBuildCatchupScript_PluginWithSingleQuote)
	for _, p := range plugins {
		expected := "echo " + shellQuote("==> Resetting "+p+"...")
		if !strings.Contains(script, expected) {
			t.Errorf("missing echo for plugin %s", p)
		}
	}
}

func TestBuildCatchupScript_PluginWithSingleQuote(t *testing.T) {
	t.Parallel()

	plugins := []string{"plugins/it's-a-test"}
	script := buildCatchupScript("/var/www/discourse", plugins)

	// The cd path should be properly quoted
	if !strings.Contains(script, "cd "+shellQuote("plugins/it's-a-test")) {
		t.Error("plugin path with single quote not properly quoted in cd")
	}

	// The echo message should also be properly quoted via shellQuote
	expected := "echo " + shellQuote("==> Resetting plugins/it's-a-test...")
	if !strings.Contains(script, expected) {
		t.Errorf("plugin echo with single quote not properly quoted\nwant substring: %s\ngot script:\n%s", expected, script)
	}
}

func TestBuildCatchupScript_CustomWorkdir(t *testing.T) {
	t.Parallel()

	script := buildCatchupScript("/custom/workdir", []string{"plugins/foo"})

	if !strings.Contains(script, "cd "+shellQuote("/custom/workdir")) {
		t.Error("should cd back to custom workdir after plugin")
	}
}

func TestBuildCatchupScript_NoGitPull(t *testing.T) {
	t.Parallel()

	// Verify that neither core nor plugin sections use "git pull"
	plugins := []string{"plugins/discourse-ai"}
	script := buildCatchupScript("/var/www/discourse", plugins)

	for i, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "git pull" || trimmed == "git pull --ff-only" {
			t.Errorf("line %d uses git pull (%q); should use git fetch + git reset --hard @{u}", i+1, trimmed)
		}
	}
}
