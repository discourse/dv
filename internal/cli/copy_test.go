package cli

import "testing"

// The mkdir behind these paths needs a live container, so it is untested here.
func TestContainerParentDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "nested file destination",
			input:    "/home/discourse/.config/term-llm/mcp.json",
			expected: "/home/discourse/.config/term-llm",
		},
		{
			name:     "shallow file destination",
			input:    "/tmp/file.txt",
			expected: "/tmp",
		},
		{
			name:     "trailing slash means the destination is a directory",
			input:    "/home/discourse/.config/claude/",
			expected: "/home/discourse/.config/claude",
		},
		{
			name:     "destination at filesystem root needs no directory",
			input:    "/foo",
			expected: "",
		},
		{
			name:     "filesystem root itself",
			input:    "/",
			expected: "",
		},
		{
			name:     "doubled root separators",
			input:    "//",
			expected: "",
		},
		{
			name:     "path containing a space",
			input:    "/home/discourse/My Documents/config.json",
			expected: "/home/discourse/My Documents",
		},
		{
			name:     "path containing a single quote is preserved verbatim",
			input:    "/home/discourse/.config/dv-it's/settings.json",
			expected: "/home/discourse/.config/dv-it's",
		},
		{
			name:     "path containing shell metacharacters is preserved verbatim",
			input:    "/home/discourse/backup$(date)/file.txt",
			expected: "/home/discourse/backup$(date)",
		},
		{
			name:     "bare relative name has no parent to create",
			input:    "file.txt",
			expected: "",
		},
		{
			name:     "dot-relative destination has no parent to create",
			input:    "./settings.json",
			expected: "",
		},
		{
			name:     "relative path with directories",
			input:    "plugins/hello/index.js",
			expected: "plugins/hello",
		},
		{
			name:     "empty destination",
			input:    "",
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := containerParentDir(tc.input); got != tc.expected {
				t.Errorf("containerParentDir(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}
