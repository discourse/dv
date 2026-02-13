package cli

import (
	"testing"
)

func TestClassifySession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command string
		want    string
	}{
		{
			name:    "empty command",
			command: "",
			want:    "unknown",
		},
		{
			name:    "bare bash",
			command: "bash -l",
			want:    "shell",
		},
		{
			name:    "full path bash",
			command: "/bin/bash -l",
			want:    "shell",
		},
		{
			name:    "zsh shell",
			command: "/bin/zsh",
			want:    "shell",
		},
		{
			name:    "fish shell",
			command: "fish",
			want:    "shell",
		},
		{
			name:    "claude agent",
			command: "claude --dangerously-skip-permissions",
			want:    "agent: claude",
		},
		{
			name:    "claude with full path",
			command: "/home/discourse/.local/bin/claude -p fix the bug",
			want:    "agent: claude",
		},
		{
			name:    "codex agent",
			command: "codex exec -s danger-full-access some prompt",
			want:    "agent: codex",
		},
		{
			name:    "aider agent",
			command: "aider --yes-always --message fix it",
			want:    "agent: aider",
		},
		{
			name:    "cursor agent",
			command: "cursor-agent -f -p prompt",
			want:    "process",
		},
		{
			name:    "gemini agent",
			command: "gemini -y -p do things",
			want:    "agent: gemini",
		},
		{
			name:    "unknown process",
			command: "python3 server.py",
			want:    "process",
		},
		{
			name:    "supervisord",
			command: "/usr/bin/supervisord -n",
			want:    "process",
		},
		{
			name:    "case insensitive",
			command: "Claude --dangerously-skip-permissions",
			want:    "agent: claude",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifySession(tt.command)
			if got != tt.want {
				t.Errorf("classifySession(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

func TestTruncateCmd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{
			name:   "short string unchanged",
			input:  "bash -l",
			maxLen: 40,
			want:   "bash -l",
		},
		{
			name:   "exact length unchanged",
			input:  "1234567890",
			maxLen: 10,
			want:   "1234567890",
		},
		{
			name:   "long string truncated",
			input:  "claude --dangerously-skip-permissions -p fix the bug",
			maxLen: 20,
			want:   "claude --dangerou...",
		},
		{
			name:   "empty string",
			input:  "",
			maxLen: 10,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := truncateCmd(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncateCmd(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}
