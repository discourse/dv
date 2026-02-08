package session

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCurrentKey(t *testing.T) {
	t.Parallel()

	key, err := CurrentKey()
	if err != nil {
		t.Fatalf("CurrentKey() error = %v", err)
	}
	if !strings.HasPrefix(key, "tty:") && !strings.HasPrefix(key, "sid:") {
		t.Fatalf("CurrentKey() = %q, want prefix tty: or sid:", key)
	}
}

func TestSetGetClearCurrentAgent(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	if err := SetCurrentAgent("agent-one"); err != nil {
		t.Fatalf("SetCurrentAgent() error = %v", err)
	}
	if got := GetCurrentAgent(); got != "agent-one" {
		t.Fatalf("GetCurrentAgent() = %q, want %q", got, "agent-one")
	}
	if err := ClearCurrentAgent(); err != nil {
		t.Fatalf("ClearCurrentAgent() error = %v", err)
	}
	if got := GetCurrentAgent(); got != "" {
		t.Fatalf("GetCurrentAgent() after clear = %q, want empty", got)
	}
}

func TestSaveNormalizesLegacySIDKeys(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	sid, err := CurrentSID()
	if err != nil {
		t.Fatalf("CurrentSID() error = %v", err)
	}
	state := &State{
		Sessions: map[string]string{
			strconv.Itoa(sid): "legacy-agent",
		},
	}
	if err := state.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	path, err := sessionsPath()
	if err != nil {
		t.Fatalf("sessionsPath() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	content := string(data)
	if strings.Contains(content, `"`+strconv.Itoa(sid)+`"`) {
		t.Fatalf("Save() preserved legacy SID key in %s: %s", filepath.Base(path), content)
	}
	if !strings.Contains(content, `"sid:`) {
		t.Fatalf("Save() did not normalize to sid:* key in %s: %s", filepath.Base(path), content)
	}
}

func TestLoadCorruptFileReturnsEmptyState(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	path := filepath.Join(runtimeDir, "dv", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	state, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(state.Sessions) != 0 {
		t.Fatalf("Load() sessions len = %d, want 0", len(state.Sessions))
	}
}
