package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCurrentKey(t *testing.T) {
	t.Parallel()

	key := CurrentKey()
	want := pidKey(os.Getppid())
	if key != want {
		t.Fatalf("CurrentKey() = %q, want %q", key, want)
	}
}

func TestParentPID(t *testing.T) {
	t.Parallel()

	got := parentPID(os.Getpid())
	if got != os.Getppid() {
		t.Fatalf("parentPID(%d) = %d, want %d", os.Getpid(), got, os.Getppid())
	}
}

func TestParentPID_DeadProcess(t *testing.T) {
	t.Parallel()

	got := parentPID(2147483647)
	if got != -1 {
		t.Fatalf("parentPID(2147483647) = %d, want -1", got)
	}
}

func TestWalkAncestors(t *testing.T) {
	t.Parallel()

	ppid := os.Getppid()
	chain := walkAncestors(ppid)
	if len(chain) == 0 {
		t.Fatal("walkAncestors returned empty chain")
	}
	if chain[0] != ppid {
		t.Fatalf("walkAncestors()[0] = %d, want %d", chain[0], ppid)
	}
	seen := map[int]bool{}
	for _, pid := range chain {
		if seen[pid] {
			t.Fatalf("walkAncestors contains duplicate PID %d", pid)
		}
		seen[pid] = true
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

func TestGetCurrentAgent_WalksAncestors(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	grandparent := parentPID(os.Getppid())
	if grandparent <= 1 {
		t.Skip("cannot determine grandparent PID")
	}

	path := filepath.Join(runtimeDir, "dv", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	gpKey := pidKey(grandparent)
	state := &State{Sessions: map[string]string{gpKey: "ancestor-agent"}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("json.Marshal error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	got := GetCurrentAgent()
	if got != "ancestor-agent" {
		t.Fatalf("GetCurrentAgent() = %q, want %q", got, "ancestor-agent")
	}
}

func TestGetCurrentAgent_PrefersNearestAncestor(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	ppid := os.Getppid()
	grandparent := parentPID(ppid)
	if grandparent <= 1 {
		t.Skip("cannot determine grandparent PID")
	}

	path := filepath.Join(runtimeDir, "dv", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	state := &State{Sessions: map[string]string{
		pidKey(ppid):        "parent-agent",
		pidKey(grandparent): "grandparent-agent",
	}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("json.Marshal error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	got := GetCurrentAgent()
	if got != "parent-agent" {
		t.Fatalf("GetCurrentAgent() = %q, want %q (nearest ancestor)", got, "parent-agent")
	}
}

func TestSetCurrentAgent_UpdatesAncestorKey(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	grandparent := parentPID(os.Getppid())
	if grandparent <= 1 {
		t.Skip("cannot determine grandparent PID")
	}

	// Seed only grandparent key (simulates nested shell inheriting ancestor selection).
	path := filepath.Join(runtimeDir, "dv", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	gpKey := pidKey(grandparent)
	state := &State{Sessions: map[string]string{gpKey: "old-agent"}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("json.Marshal error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	// SetCurrentAgent should update the grandparent key, not create a new parent key.
	if err := SetCurrentAgent("new-agent"); err != nil {
		t.Fatalf("SetCurrentAgent() error = %v", err)
	}

	got := GetCurrentAgent()
	if got != "new-agent" {
		t.Fatalf("GetCurrentAgent() = %q, want %q", got, "new-agent")
	}

	// Verify it updated the grandparent key, not created a shadow parent key.
	loaded, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("loadFromPath error = %v", err)
	}
	if loaded.Get(gpKey) != "new-agent" {
		t.Fatalf("grandparent key = %q, want %q", loaded.Get(gpKey), "new-agent")
	}
	ppidKey := pidKey(os.Getppid())
	if loaded.Get(ppidKey) != "" {
		t.Fatalf("parent key should not exist, got %q", loaded.Get(ppidKey))
	}
}

func TestClearCurrentAgent_RemovesAllAncestorKeys(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	ppid := os.Getppid()
	grandparent := parentPID(ppid)
	if grandparent <= 1 {
		t.Skip("cannot determine grandparent PID")
	}

	// Seed both parent and grandparent keys.
	path := filepath.Join(runtimeDir, "dv", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	state := &State{Sessions: map[string]string{
		pidKey(ppid):        "parent-agent",
		pidKey(grandparent): "grandparent-agent",
	}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("json.Marshal error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	if err := ClearCurrentAgent(); err != nil {
		t.Fatalf("ClearCurrentAgent() error = %v", err)
	}

	// After clear, no ancestor should provide a fallback.
	got := GetCurrentAgent()
	if got != "" {
		t.Fatalf("GetCurrentAgent() after clear = %q, want empty", got)
	}
}

func TestSetCurrentAgent_EmptyClearsAllAncestors(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	ppid := os.Getppid()
	grandparent := parentPID(ppid)
	if grandparent <= 1 {
		t.Skip("cannot determine grandparent PID")
	}

	// Seed both parent and grandparent keys.
	path := filepath.Join(runtimeDir, "dv", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	state := &State{Sessions: map[string]string{
		pidKey(ppid):        "parent-agent",
		pidKey(grandparent): "grandparent-agent",
	}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("json.Marshal error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	// SetCurrentAgent("") should behave like ClearCurrentAgent.
	if err := SetCurrentAgent(""); err != nil {
		t.Fatalf("SetCurrentAgent(\"\") error = %v", err)
	}

	got := GetCurrentAgent()
	if got != "" {
		t.Fatalf("GetCurrentAgent() after SetCurrentAgent(\"\") = %q, want empty", got)
	}
}

func TestNormalizeAndClean_LivePID(t *testing.T) {
	t.Parallel()

	key := pidKey(os.Getpid())
	state := &State{Sessions: map[string]string{key: "my-agent"}}
	normalizeAndClean(state)

	if agent, ok := state.Sessions[key]; !ok || agent != "my-agent" {
		t.Fatalf("normalizeAndClean removed live pid entry; sessions = %v", state.Sessions)
	}
}

func TestNormalizeAndClean_StalePID(t *testing.T) {
	t.Parallel()

	key := "pid:2147483647"
	state := &State{Sessions: map[string]string{key: "stale-agent"}}
	changed := normalizeAndClean(state)

	if !changed {
		t.Fatal("normalizeAndClean should report changed=true for stale PID")
	}
	if _, ok := state.Sessions[key]; ok {
		t.Fatalf("normalizeAndClean kept stale pid entry; sessions = %v", state.Sessions)
	}
}

func TestNormalizeAndClean_DropsLegacyKeys(t *testing.T) {
	t.Parallel()

	state := &State{Sessions: map[string]string{
		"sid:12345":           "a",
		"12345":               "b",
		"tty:1280:sid:12345":  "c",
		"tty:1280:ppid:12345": "d",
		pidKey(os.Getpid()):   "keeper",
	}}
	changed := normalizeAndClean(state)

	if !changed {
		t.Fatal("normalizeAndClean should report changed=true when dropping legacy keys")
	}
	if len(state.Sessions) != 1 {
		t.Fatalf("expected 1 session after cleanup, got %d: %v", len(state.Sessions), state.Sessions)
	}
	if state.Sessions[pidKey(os.Getpid())] != "keeper" {
		t.Fatal("normalizeAndClean dropped the valid pid entry")
	}
}

func TestNormalizeAndClean_DetectsPIDReuse(t *testing.T) {
	t.Parallel()

	pid := os.Getpid()
	st := processStartTime(pid)
	if st <= 0 {
		t.Skip("processStartTime not available")
	}
	// Key with correct PID but wrong start time simulates PID reuse.
	staleKey := fmt.Sprintf("pid:%d:%d", pid, st+999999)
	state := &State{Sessions: map[string]string{staleKey: "reused-agent"}}
	changed := normalizeAndClean(state)

	if !changed {
		t.Fatal("normalizeAndClean should detect PID reuse via start time mismatch")
	}
	if len(state.Sessions) != 0 {
		t.Fatalf("expected 0 sessions after PID reuse cleanup, got %v", state.Sessions)
	}
}

func TestNormalizeAndClean_CanonicalizesKey(t *testing.T) {
	t.Parallel()

	pid := os.Getpid()
	st := processStartTime(pid)
	if st <= 0 {
		t.Skip("processStartTime not available")
	}
	nonCanonical := fmt.Sprintf("pid:0%d:%d", pid, st)
	state := &State{Sessions: map[string]string{nonCanonical: "my-agent"}}
	changed := normalizeAndClean(state)

	if !changed {
		t.Fatal("normalizeAndClean should report changed when canonicalizing key")
	}
	canonical := pidKey(pid)
	if state.Sessions[canonical] != "my-agent" {
		t.Fatalf("expected canonical key %q with agent, got sessions %v", canonical, state.Sessions)
	}
	if _, ok := state.Sessions[nonCanonical]; ok {
		t.Fatal("non-canonical key should have been removed")
	}
}

func TestNormalizeAndClean_DropsPIDOnlyWhenStartTimeAvailable(t *testing.T) {
	t.Parallel()

	pid := os.Getpid()
	if processStartTime(pid) <= 0 {
		t.Skip("processStartTime not available")
	}
	// PID-only key for a live process whose start time IS available.
	pidOnly := fmt.Sprintf("pid:%d", pid)
	state := &State{Sessions: map[string]string{pidOnly: "unverified-agent"}}
	changed := normalizeAndClean(state)

	if !changed {
		t.Fatal("normalizeAndClean should drop PID-only entries when start time is available")
	}
	if len(state.Sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %v", state.Sessions)
	}
}

func TestNormalizeAndClean_RejectsPIDZeroAndOne(t *testing.T) {
	t.Parallel()

	state := &State{Sessions: map[string]string{
		"pid:0":             "a",
		"pid:1":             "b",
		"pid:-5":            "c",
		pidKey(os.Getpid()): "keeper",
	}}
	changed := normalizeAndClean(state)

	if !changed {
		t.Fatal("normalizeAndClean should report changed when rejecting invalid PIDs")
	}
	if len(state.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d: %v", len(state.Sessions), state.Sessions)
	}
	if state.Sessions[pidKey(os.Getpid())] != "keeper" {
		t.Fatal("normalizeAndClean dropped the valid pid entry")
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

// Tests below override processStartTimeFn and must NOT use t.Parallel().

func TestGetCurrentAgent_DegradedStartTime(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	ppid := os.Getppid()
	realST := processStartTime(ppid)
	if realST <= 0 {
		t.Skip("processStartTime not available")
	}

	// Write a key with start time while processStartTime still works.
	path := filepath.Join(runtimeDir, "dv", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	key := fmt.Sprintf("pid:%d:%d", ppid, realST)
	state := &State{Sessions: map[string]string{key: "test-agent"}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("json.Marshal error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	// Simulate processStartTime failure — pidKey would produce pid:<ppid>
	// which wouldn't match the stored pid:<ppid>:<st> key.
	// findByPID matches by parsed PID, so lookup should still succeed.
	processStartTimeFn = func(int) int64 { return 0 }
	t.Cleanup(func() { processStartTimeFn = processStartTime })

	got := GetCurrentAgent()
	if got != "test-agent" {
		t.Fatalf("GetCurrentAgent() with degraded start time = %q, want %q", got, "test-agent")
	}
}

func TestClearCurrentAgent_DegradedStartTime(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	ppid := os.Getppid()
	realST := processStartTime(ppid)
	if realST <= 0 {
		t.Skip("processStartTime not available")
	}

	// Write a key with start time.
	path := filepath.Join(runtimeDir, "dv", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	key := fmt.Sprintf("pid:%d:%d", ppid, realST)
	state := &State{Sessions: map[string]string{key: "test-agent"}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("json.Marshal error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	// Simulate processStartTime failure.
	processStartTimeFn = func(int) int64 { return 0 }
	t.Cleanup(func() { processStartTimeFn = processStartTime })

	// Clear should remove the entry via PID-based matching despite
	// not being able to recompute the exact stored key.
	if err := ClearCurrentAgent(); err != nil {
		t.Fatalf("ClearCurrentAgent() error = %v", err)
	}

	got := GetCurrentAgent()
	if got != "" {
		t.Fatalf("GetCurrentAgent() after degraded clear = %q, want empty", got)
	}
}

func TestSetCurrentAgent_DegradedStartTime(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	ppid := os.Getppid()
	realST := processStartTime(ppid)
	if realST <= 0 {
		t.Skip("processStartTime not available")
	}

	// Write a key with start time.
	path := filepath.Join(runtimeDir, "dv", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	key := fmt.Sprintf("pid:%d:%d", ppid, realST)
	state := &State{Sessions: map[string]string{key: "old-agent"}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("json.Marshal error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	// Simulate processStartTime failure.
	processStartTimeFn = func(int) int64 { return 0 }
	t.Cleanup(func() { processStartTimeFn = processStartTime })

	// SetCurrentAgent should find and update the existing key by PID match.
	if err := SetCurrentAgent("new-agent"); err != nil {
		t.Fatalf("SetCurrentAgent() error = %v", err)
	}

	got := GetCurrentAgent()
	if got != "new-agent" {
		t.Fatalf("GetCurrentAgent() after degraded set = %q, want %q", got, "new-agent")
	}
}
