package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePluginSpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		repo  string
		path  string
	}{
		{"discourse-kanban", "https://github.com/discourse/discourse-kanban.git", "plugins/discourse-kanban"},
		{"discourse-kanban.git", "https://github.com/discourse/discourse-kanban.git", "plugins/discourse-kanban"},
		{"discourse/discourse-kanban", "https://github.com/discourse/discourse-kanban.git", "plugins/discourse-kanban"},
		{"discourse/discourse-kanban.git", "https://github.com/discourse/discourse-kanban.git", "plugins/discourse-kanban"},
		{"https://github.com/discourse/discourse-kanban.git", "https://github.com/discourse/discourse-kanban.git", "plugins/discourse-kanban"},
		{"git@github.com:discourse/discourse-kanban.git", "git@github.com:discourse/discourse-kanban.git", "plugins/discourse-kanban"},
		{"ssh://git@github.com/discourse/discourse-kanban.git", "ssh://git@github.com/discourse/discourse-kanban.git", "plugins/discourse-kanban"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got, err := resolvePluginSpec(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			if got.Repo != tt.repo || got.Path != tt.path {
				t.Fatalf("resolvePluginSpec(%q) = repo %q path %q, want repo %q path %q", tt.input, got.Repo, got.Path, tt.repo, tt.path)
			}
		})
	}
}

func TestResolvePluginSpecsRejectsPathCollision(t *testing.T) {
	t.Parallel()

	_, err := resolvePluginSpecs([]string{"discourse/foo", "other/foo"})
	if err == nil {
		t.Fatal("expected collision error")
	}
}

func TestResolvePluginSpecRejectsInvalidSpecs(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "   ", "one/two/three"} {
		input := input
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			if _, err := resolvePluginSpec(input); err == nil {
				t.Fatalf("expected error for %q", input)
			}
		})
	}
}

func TestResolveLocalPluginMount(t *testing.T) {
	t.Parallel()

	mk := func(t *testing.T, rb string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "plugin.rb"), []byte(rb), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("name from plugin.rb", func(t *testing.T) {
		dir := mk(t, "# frozen_string_literal: true\n# name: second-brain\n# about: x\n")
		got, name, err := resolveLocalPluginMount(dir, "/var/www/discourse")
		if err != nil {
			t.Fatal(err)
		}
		if name != "second-brain" || got.Host != dir || got.Container != "/var/www/discourse/plugins/second-brain" {
			t.Fatalf("got %+v name=%q", got, name)
		}
	})

	t.Run("falls back to directory name", func(t *testing.T) {
		dir := mk(t, "# about: no name line here\n")
		got, name, err := resolveLocalPluginMount(dir, "/var/www/discourse")
		if err != nil {
			t.Fatal(err)
		}
		if name != filepath.Base(dir) || got.Container != "/var/www/discourse/plugins/"+filepath.Base(dir) {
			t.Fatalf("expected fallback to dir name, got %+v name=%q", got, name)
		}
	})

	t.Run("rejects a name that escapes plugins dir", func(t *testing.T) {
		dir := mk(t, "# name: ../../etc/cron.d/x\n")
		if _, _, err := resolveLocalPluginMount(dir, "/var/www/discourse"); err == nil {
			t.Fatal("expected error for a plugin.rb name containing path separators")
		}
	})

	t.Run("errors without plugin.rb", func(t *testing.T) {
		if _, _, err := resolveLocalPluginMount(t.TempDir(), "/var/www/discourse"); err == nil {
			t.Fatal("expected error for directory without plugin.rb")
		}
	})

	t.Run("errors on missing path", func(t *testing.T) {
		if _, _, err := resolveLocalPluginMount(filepath.Join(t.TempDir(), "nope"), "/var/www/discourse"); err == nil {
			t.Fatal("expected error for missing path")
		}
	})
}

func TestPluginNameFromRB(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"# name: second-brain\n":          "second-brain",
		"#name:second-brain\n":            "second-brain",
		"#   name:   spaced-out  \n":      "spaced-out",
		"# frozen_string_literal: true\n": "",
		"no comments at all\n":            "",
	}
	for in, want := range cases {
		if got := pluginNameFromRB([]byte(in)); got != want {
			t.Errorf("pluginNameFromRB(%q) = %q, want %q", in, got, want)
		}
	}
}
