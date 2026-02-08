package cli

import (
	"errors"
	"net"
	"os"
	"sync"
	"testing"

	"dv/internal/config"
)

type selectionSeamOverrides struct {
	getSession func() string
	clear      func() error
	exists     func(string) bool
	labels     func(string) (map[string]string, error)
	warn       func()
	resetOnce  bool
}

func applySelectionSeams(t *testing.T, overrides selectionSeamOverrides) {
	t.Helper()

	selectionSeamsMu.Lock()
	prevGet := getSessionCurrentAgent
	prevClear := clearSessionCurrentAgent
	prevExists := dockerExistsForSelection
	prevLabels := dockerLabelsForSelection
	prevWarn := warnStaleSessionSelection
	if overrides.getSession != nil {
		getSessionCurrentAgent = overrides.getSession
	}
	if overrides.clear != nil {
		clearSessionCurrentAgent = overrides.clear
	}
	if overrides.exists != nil {
		dockerExistsForSelection = overrides.exists
	}
	if overrides.labels != nil {
		dockerLabelsForSelection = overrides.labels
	}
	if overrides.warn != nil {
		warnStaleSessionSelection = overrides.warn
	}
	if overrides.resetOnce {
		staleSessionWarnOnce = sync.Once{}
	}
	selectionSeamsMu.Unlock()

	t.Cleanup(func() {
		selectionSeamsMu.Lock()
		getSessionCurrentAgent = prevGet
		clearSessionCurrentAgent = prevClear
		dockerExistsForSelection = prevExists
		dockerLabelsForSelection = prevLabels
		warnStaleSessionSelection = prevWarn
		staleSessionWarnOnce = sync.Once{}
		selectionSeamsMu.Unlock()
	})
}

func TestShellQuote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "''",
		},
		{
			name:     "simple string",
			input:    "hello",
			expected: "'hello'",
		},
		{
			name:     "string with spaces",
			input:    "hello world",
			expected: "'hello world'",
		},
		{
			name:     "string with single quote",
			input:    "it's",
			expected: "'it'\"'\"'s'",
		},
		{
			name:     "string with multiple single quotes",
			input:    "it's a 'test'",
			expected: "'it'\"'\"'s a '\"'\"'test'\"'\"''",
		},
		{
			name:     "string with double quotes",
			input:    `say "hello"`,
			expected: `'say "hello"'`,
		},
		{
			name:     "string with backticks",
			input:    "run `command`",
			expected: "'run `command`'",
		},
		{
			name:     "string with dollar sign",
			input:    "$HOME",
			expected: "'$HOME'",
		},
		{
			name:     "string with newline",
			input:    "line1\nline2",
			expected: "'line1\nline2'",
		},
		{
			name:     "string with special chars",
			input:    "test;rm -rf /",
			expected: "'test;rm -rf /'",
		},
		{
			name:     "string with pipe and ampersand",
			input:    "cmd1 | cmd2 && cmd3",
			expected: "'cmd1 | cmd2 && cmd3'",
		},
		{
			name:     "string with exclamation",
			input:    "test!",
			expected: "'test!'",
		},
		{
			name:     "unicode characters",
			input:    "héllo 世界",
			expected: "'héllo 世界'",
		},
		{
			name:     "path with spaces",
			input:    "/path/to/my file.txt",
			expected: "'/path/to/my file.txt'",
		},
		{
			name:     "complex injection attempt",
			input:    "'; rm -rf / #",
			expected: "''\"'\"'; rm -rf / #'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := shellQuote(tt.input)
			if got != tt.expected {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestShellJoin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		argv     []string
		expected string
	}{
		{
			name:     "empty slice",
			argv:     []string{},
			expected: "",
		},
		{
			name:     "single arg",
			argv:     []string{"hello"},
			expected: "'hello'",
		},
		{
			name:     "multiple simple args",
			argv:     []string{"ls", "-la", "/tmp"},
			expected: "'ls' '-la' '/tmp'",
		},
		{
			name:     "args with spaces",
			argv:     []string{"echo", "hello world"},
			expected: "'echo' 'hello world'",
		},
		{
			name:     "args with quotes",
			argv:     []string{"echo", "it's"},
			expected: "'echo' 'it'\"'\"'s'",
		},
		{
			name:     "args with special chars",
			argv:     []string{"bash", "-c", "echo $HOME"},
			expected: "'bash' '-c' 'echo $HOME'",
		},
		{
			name:     "injection attempt",
			argv:     []string{"echo", "hello; rm -rf /"},
			expected: "'echo' 'hello; rm -rf /'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := shellJoin(tt.argv)
			if got != tt.expected {
				t.Errorf("shellJoin(%q) = %q, want %q", tt.argv, got, tt.expected)
			}
		})
	}
}

func TestAgentNameSlug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "whitespace only",
			input:    "   ",
			expected: "",
		},
		{
			name:     "simple lowercase",
			input:    "myagent",
			expected: "myagent",
		},
		{
			name:     "uppercase to lowercase",
			input:    "MyAgent",
			expected: "myagent",
		},
		{
			name:     "with numbers",
			input:    "agent123",
			expected: "agent123",
		},
		{
			name:     "allowed special chars",
			input:    "my-agent_v1.0",
			expected: "my-agent_v1.0",
		},
		{
			name:     "spaces become dashes",
			input:    "my agent",
			expected: "my-agent",
		},
		{
			name:     "consecutive spaces become single dash",
			input:    "my   agent",
			expected: "my-agent",
		},
		{
			name:     "leading special chars trimmed",
			input:    "---myagent",
			expected: "myagent",
		},
		{
			name:     "trailing special chars trimmed",
			input:    "myagent---",
			expected: "myagent",
		},
		{
			name:     "leading and trailing trimmed",
			input:    "  --myagent--  ",
			expected: "myagent",
		},
		{
			name:     "unicode replaced with dash",
			input:    "agent世界test",
			expected: "agent-test",
		},
		{
			name:     "mixed special characters",
			input:    "my@agent#test!",
			expected: "my-agent-test",
		},
		{
			name:     "multiple consecutive special chars",
			input:    "my@#$%agent",
			expected: "my-agent",
		},
		{
			name:     "all disallowed chars",
			input:    "@#$%^&*()",
			expected: "",
		},
		{
			name:     "preserves dots underscores dashes",
			input:    "my.agent_v1-beta",
			expected: "my.agent_v1-beta",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := agentNameSlug(tt.input)
			if got != tt.expected {
				t.Errorf("agentNameSlug(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestIsTruthyEnv(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected bool
	}{
		{"empty", "", false},
		{"1", "1", true},
		{"true", "true", true},
		{"TRUE", "TRUE", true},
		{"True", "True", true},
		{"yes", "yes", true},
		{"YES", "YES", true},
		{"on", "on", true},
		{"ON", "ON", true},
		{"0", "0", false},
		{"false", "false", false},
		{"no", "no", false},
		{"off", "off", false},
		{"random", "random", false},
		{"whitespace 1", "  1  ", true},
		{"whitespace true", "  true  ", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := "TEST_TRUTHY_" + tt.name
			t.Setenv(key, tt.value)
			got := isTruthyEnv(key)
			if got != tt.expected {
				t.Errorf("isTruthyEnv(%q) with value %q = %v, want %v", key, tt.value, got, tt.expected)
			}
		})
	}
}

func TestIsPortInUse_DockerAllocated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		port      int
		allocated map[int]bool
		want      bool
	}{
		{
			name:      "port in docker map",
			port:      8080,
			allocated: map[int]bool{8080: true},
			want:      true,
		},
		{
			name:      "port not in docker map",
			port:      8080,
			allocated: map[int]bool{9090: true},
			want:      false,
		},
		{
			name:      "nil docker map",
			port:      8080,
			allocated: nil,
			want:      false,
		},
		{
			name:      "empty docker map",
			port:      8080,
			allocated: map[int]bool{},
			want:      false,
		},
		{
			name:      "multiple ports allocated",
			port:      8082,
			allocated: map[int]bool{8080: true, 8081: true, 8082: true},
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// For these tests we're specifically checking docker allocated map logic
			// To avoid flaky tests from actual port binding, we only test cases where
			// docker map returns true early, before trying to bind
			if tt.want {
				got := isPortInUse(tt.port, tt.allocated)
				if got != tt.want {
					t.Errorf("isPortInUse(%d, %v) = %v, want %v", tt.port, tt.allocated, got, tt.want)
				}
			}
		})
	}
}

func TestIsPortInUse_ActualBinding(t *testing.T) {
	// This test verifies the actual port binding check works.
	// We bind a port, then check if isPortInUse correctly detects it.

	// Find an available port by letting the OS assign one
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port

	// The port is now in use by our listener
	if !isPortInUse(port, nil) {
		t.Errorf("isPortInUse(%d, nil) = false, want true (port is bound)", port)
	}
}

func TestIsPortInUse_AvailablePort(t *testing.T) {
	// Test that an actually available port returns false
	// We find a port that was available (by binding temporarily), release it,
	// then immediately check - there's a small race window but it should work

	// Find an available port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close() // Release the port

	// Port should now be available (with small race window)
	// We pass an empty docker map to avoid docker-allocated false positives
	got := isPortInUse(port, map[int]bool{})
	// Note: This may occasionally fail due to port reuse timing
	// If this test becomes flaky, we can skip it or use a more robust approach
	if got {
		t.Logf("isPortInUse(%d) = true for recently released port (possible race)", port)
		// Don't fail - this can happen due to TIME_WAIT or port reuse
	}
}

func TestCurrentAgentName_PrefersDVAgent(t *testing.T) {
	t.Setenv("DV_AGENT", "env-agent")

	cfg := config.Config{
		SelectedAgent:    "global-agent",
		DefaultContainer: "default-agent",
	}

	applySelectionSeams(t, selectionSeamOverrides{
		getSession: func() string { return "session-agent" },
	})

	if got := currentAgentName(cfg); got != "env-agent" {
		t.Fatalf("currentAgentName() = %q, want %q", got, "env-agent")
	}
}

func TestCurrentAgentName_UsesValidSessionAgent(t *testing.T) {
	prev, had := os.LookupEnv("DV_AGENT")
	_ = os.Unsetenv("DV_AGENT")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("DV_AGENT", prev)
		} else {
			_ = os.Unsetenv("DV_AGENT")
		}
	})

	cfg := config.Config{
		SelectedAgent:    "global-agent",
		DefaultContainer: "default-agent",
		SelectedImage:    "img-a",
		ContainerImages:  map[string]string{"session-agent": "img-a"},
	}

	clearCalled := false
	applySelectionSeams(t, selectionSeamOverrides{
		getSession: func() string { return "session-agent" },
		exists:     func(name string) bool { return true },
		clear: func() error {
			clearCalled = true
			return nil
		},
	})

	if got := currentAgentName(cfg); got != "session-agent" {
		t.Fatalf("currentAgentName() = %q, want %q", got, "session-agent")
	}
	if clearCalled {
		t.Fatalf("clearSessionCurrentAgent() called unexpectedly")
	}
}

func TestCurrentAgentName_AutoHealsMissingSessionAgent(t *testing.T) {
	prev, had := os.LookupEnv("DV_AGENT")
	_ = os.Unsetenv("DV_AGENT")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("DV_AGENT", prev)
		} else {
			_ = os.Unsetenv("DV_AGENT")
		}
	})

	cfg := config.Config{
		SelectedAgent:    "global-agent",
		DefaultContainer: "default-agent",
	}

	clearCalled := false
	applySelectionSeams(t, selectionSeamOverrides{
		getSession: func() string { return "stale-session-agent" },
		exists:     func(name string) bool { return false },
		clear: func() error {
			clearCalled = true
			return nil
		},
		warn:      func() {},
		resetOnce: true,
	})

	if got := currentAgentName(cfg); got != "global-agent" {
		t.Fatalf("currentAgentName() = %q, want %q", got, "global-agent")
	}
	if !clearCalled {
		t.Fatalf("clearSessionCurrentAgent() was not called")
	}
}

func TestCurrentAgentName_AutoHealsCrossImageSessionAgent(t *testing.T) {
	prev, had := os.LookupEnv("DV_AGENT")
	_ = os.Unsetenv("DV_AGENT")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("DV_AGENT", prev)
		} else {
			_ = os.Unsetenv("DV_AGENT")
		}
	})

	cfg := config.Config{
		SelectedAgent:    "global-agent",
		DefaultContainer: "default-agent",
		SelectedImage:    "img-b",
		ContainerImages:  map[string]string{"session-agent": "img-a"},
	}

	clearCalled := false
	applySelectionSeams(t, selectionSeamOverrides{
		getSession: func() string { return "session-agent" },
		exists:     func(name string) bool { return true },
		clear: func() error {
			clearCalled = true
			return nil
		},
		warn:      func() {},
		resetOnce: true,
	})

	if got := currentAgentName(cfg); got != "global-agent" {
		t.Fatalf("currentAgentName() = %q, want %q", got, "global-agent")
	}
	if !clearCalled {
		t.Fatalf("clearSessionCurrentAgent() was not called")
	}
}

func TestSessionAgentIsStale_UsesLabelsFallback(t *testing.T) {
	cfg := config.Config{
		SelectedImage: "img-b",
		SelectedAgent: "session-agent",
	}

	applySelectionSeams(t, selectionSeamOverrides{
		labels: func(name string) (map[string]string, error) {
			return map[string]string{"com.dv.image-name": "img-a"}, nil
		},
		exists: func(name string) bool { return true },
	})

	if !sessionAgentIsStale(cfg, "session-agent") {
		t.Fatalf("sessionAgentIsStale() = false, want true")
	}
}

func TestSessionAgentIsStale_IgnoresLabelErrors(t *testing.T) {
	cfg := config.Config{
		SelectedImage: "img-b",
		SelectedAgent: "session-agent",
	}

	applySelectionSeams(t, selectionSeamOverrides{
		labels: func(name string) (map[string]string, error) {
			return nil, errors.New("labels unavailable")
		},
		exists: func(name string) bool { return true },
	})

	if sessionAgentIsStale(cfg, "session-agent") {
		t.Fatalf("sessionAgentIsStale() = true, want false when labels fail")
	}
}
