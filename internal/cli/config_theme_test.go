package cli

import (
	"testing"
)

func TestThemeDirSlug(t *testing.T) {
	tests := map[string]string{
		"My Theme":              "my-theme",
		"already-kebab":         "already-kebab",
		"   spaces   ":          "spaces",
		"Symbols*&^":            "symbols",
		"":                      "theme",
		"Ünicode Friendly Name": "ünicode-friendly-name",
	}
	for input, expected := range tests {
		if got := themeDirSlug(input); got != expected {
			t.Fatalf("themeDirSlug(%q) = %q, want %q", input, got, expected)
		}
	}
}

func TestNormalizeThemeRepo(t *testing.T) {
	tests := []struct {
		in       string
		url      string
		basename string
	}{
		{"discourse/new-theme", "https://github.com/discourse/new-theme.git", "new-theme"},
		{"custom-theme", "https://github.com/discourse/custom-theme.git", "custom-theme"},
		{"https://example.com/foo/bar.git", "https://example.com/foo/bar.git", "bar"},
		{"git@github.com:foo/bar.git", "git@github.com:foo/bar.git", "bar"},
	}
	for _, tt := range tests {
		url, name := normalizeThemeRepo(tt.in)
		if url != tt.url || name != tt.basename {
			t.Fatalf("normalizeThemeRepo(%q) = (%q,%q) want (%q,%q)", tt.in, url, name, tt.url, tt.basename)
		}
	}
}

func TestResolveThemeSpec(t *testing.T) {
	tests := []struct {
		in   string
		repo string
		pr   int
	}{
		{
			in:   "https://github.com/discourse/discourse-mermaid-theme-component/pull/76",
			repo: "https://github.com/discourse/discourse-mermaid-theme-component.git",
			pr:   76,
		},
		{
			in:   "discourse/discourse-mermaid-theme-component#76",
			repo: "https://github.com/discourse/discourse-mermaid-theme-component.git",
			pr:   76,
		},
		{
			in:   "discourse-mermaid-theme-component#76",
			repo: "https://github.com/discourse/discourse-mermaid-theme-component.git",
			pr:   76,
		},
		{
			in:   "git@github.com:discourse/discourse-mermaid-theme-component.git#76",
			repo: "git@github.com:discourse/discourse-mermaid-theme-component.git",
			pr:   76,
		},
	}

	for _, tt := range tests {
		theme, repo, _, err := resolveThemeSpec(tt.in)
		if err != nil {
			t.Fatalf("resolveThemeSpec(%q) error = %v", tt.in, err)
		}
		if repo != tt.repo || theme.Repo != tt.repo || theme.PR != tt.pr {
			t.Fatalf("resolveThemeSpec(%q) = repo %q theme.Repo %q pr %d, want repo %q pr %d", tt.in, repo, theme.Repo, theme.PR, tt.repo, tt.pr)
		}
		if theme.Enabled == nil || !*theme.Enabled {
			t.Fatalf("resolveThemeSpec(%q) did not default Enabled to true", tt.in)
		}
	}
}

func TestResolveThemeSpecInvalidPRShorthand(t *testing.T) {
	_, _, _, err := resolveThemeSpec("discourse/foo#not-a-number")
	if err == nil {
		t.Fatal("resolveThemeSpec() error = nil, want error")
	}
}

func TestResolveThemeSpecRejectsNonGitHubPRShorthand(t *testing.T) {
	_, _, _, err := resolveThemeSpec("https://example.com/foo/bar.git#76")
	if err == nil {
		t.Fatal("resolveThemeSpec() error = nil, want GitHub PR error")
	}
}

func TestNormalizeThemeCloneSpecRejectsConflictingPRs(t *testing.T) {
	_, _, _, err := normalizeThemeCloneSpec(templateTheme{
		Repo: "https://github.com/discourse/discourse-mermaid-theme-component/pull/76",
		PR:   77,
	})
	if err == nil {
		t.Fatal("normalizeThemeCloneSpec() error = nil, want error")
	}
}

func TestResolveThemeSpecsRejectsDuplicatePaths(t *testing.T) {
	_, err := resolveThemeSpecs([]string{"discourse/foo", "foo"})
	if err == nil {
		t.Fatal("resolveThemeSpecs() error = nil, want duplicate path error")
	}
}
