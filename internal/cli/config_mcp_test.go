package cli

import "testing"

func TestHasClaudeHostCredentials(t *testing.T) {
	t.Parallel()

	t.Run("no credentials", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

		if hasClaudeHostCredentials() {
			t.Fatal("expected no Claude host credentials")
		}
	})

	t.Run("anthropic api key", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "test-key")
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

		if !hasClaudeHostCredentials() {
			t.Fatal("expected ANTHROPIC_API_KEY to count as Claude host credentials")
		}
	})

	t.Run("oauth token", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "test-token")

		if !hasClaudeHostCredentials() {
			t.Fatal("expected CLAUDE_CODE_OAUTH_TOKEN to count as Claude host credentials")
		}
	})
}
