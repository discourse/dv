package cli

import "testing"

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
