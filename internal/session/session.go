package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"dv/internal/xdg"
)

// State holds per-terminal session selections.
type State struct {
	Sessions map[string]string `json:"sessions"` // session key -> agent name
}

// Test hooks: override in tests to simulate degraded environments.
// Must not be overridden concurrently (non-parallel tests only).
var (
	parentPIDFn        = parentPID
	processStartTimeFn = processStartTime
)

func sessionsPath() (string, error) {
	runtimeDir, err := xdg.RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(runtimeDir, "sessions.json"), nil
}

// CurrentKey returns the identity key for this process's parent (the shell).
// Key shape: pid:<ppid>:<starttime> (or pid:<ppid> when start time is unavailable).
func CurrentKey() string {
	return pidKey(os.Getppid())
}

// walkAncestors returns PIDs walking up the process tree from startPID.
// Stops at PID 1 or when the chain breaks.
// parentPID is provided per-platform in parentpid_{linux,other}.go.
func walkAncestors(startPID int) []int {
	var chain []int
	seen := make(map[int]bool)
	for pid := startPID; pid > 1 && !seen[pid]; {
		seen[pid] = true
		chain = append(chain, pid)
		next := parentPIDFn(pid)
		if next <= 0 {
			break
		}
		pid = next
	}
	return chain
}

func pidKey(pid int) string {
	if st := processStartTimeFn(pid); st > 0 {
		return fmt.Sprintf("pid:%d:%d", pid, st)
	}
	return fmt.Sprintf("pid:%d", pid)
}

// parsePidKey extracts (pid, startTime) from a key like "pid:<pid>:<starttime>".
// startTime is 0 when the key has no start-time component.
func parsePidKey(key string) (pid int, startTime int64, ok bool) {
	if !strings.HasPrefix(key, "pid:") {
		return 0, 0, false
	}
	rest := key[4:]
	parts := strings.SplitN(rest, ":", 2)
	pid, err := strconv.Atoi(parts[0])
	if err != nil || pid <= 1 {
		return 0, 0, false
	}
	if len(parts) == 2 {
		st, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || st <= 0 {
			return 0, 0, false
		}
		startTime = st
	}
	return pid, startTime, true
}

func processExists(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || err == unix.EPERM
}

// Load reads the session state from disk.
func Load() (*State, error) {
	path, err := sessionsPath()
	if err != nil {
		return nil, err
	}
	return loadFromPath(path)
}

// Save writes the session state to disk, cleaning stale entries.
func (s *State) Save() error {
	path, err := sessionsPath()
	if err != nil {
		return err
	}
	return withLockedState(path, func(state *State) (bool, error) {
		state.Sessions = cloneSessions(s.Sessions)
		_ = normalizeAndClean(state)
		return true, nil
	})
}

func normalizeAndClean(s *State) (changed bool) {
	if s.Sessions == nil {
		s.Sessions = make(map[string]string)
		return true
	}

	converted := make(map[string]string, len(s.Sessions))
	for key, agent := range s.Sessions {
		key = strings.TrimSpace(key)
		agent = strings.TrimSpace(agent)
		if key == "" || agent == "" {
			changed = true
			continue
		}

		pid, storedST, ok := parsePidKey(key)
		if !ok {
			changed = true
			continue
		}

		if !processExists(pid) {
			changed = true
			continue
		}

		currentST := processStartTimeFn(pid)

		// Detect PID reuse: stored and current start times both known but differ.
		if storedST > 0 && currentST > 0 && storedST != currentST {
			changed = true
			continue
		}

		// PID-only key but start time is now available: can't verify
		// identity, drop. The user must re-select; this is a one-time
		// migration cost that prevents stale PID-only entries from being
		// silently blessed onto a reused PID.
		if storedST == 0 && currentST > 0 {
			changed = true
			continue
		}

		// Canonicalize PID formatting; preserve start-time status.
		var canonical string
		if storedST > 0 {
			canonical = fmt.Sprintf("pid:%d:%d", pid, storedST)
		} else {
			canonical = fmt.Sprintf("pid:%d", pid)
		}
		if canonical != key {
			changed = true
		}
		converted[canonical] = agent
	}
	if len(converted) != len(s.Sessions) {
		changed = true
	}
	s.Sessions = converted
	return changed
}

func lockPath(path string) string {
	return path + ".lock"
}

func withLockedState(path string, fn func(state *State) (bool, error)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lockFile, err := os.OpenFile(lockPath(path), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN) }()

	state, err := loadFromPath(path)
	if err != nil {
		return err
	}
	cleaned := normalizeAndClean(state)
	changed, err := fn(state)
	if err != nil {
		return err
	}
	if cleaned || changed {
		return saveToPathAtomic(path, state)
	}
	return nil
}

func loadFromPath(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &State{Sessions: make(map[string]string)}, nil
	}
	if err != nil {
		return nil, err
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return &State{Sessions: make(map[string]string)}, nil
	}
	if state.Sessions == nil {
		state.Sessions = make(map[string]string)
	}
	return &state, nil
}

func saveToPathAtomic(path string, state *State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Get returns the agent for the given session key, or empty if not found/stale.
func (s *State) Get(key string) string {
	if s == nil || s.Sessions == nil {
		return ""
	}
	return strings.TrimSpace(s.Sessions[key])
}

// Set stores the agent for the given session key.
func (s *State) Set(key, agent string) {
	if s.Sessions == nil {
		s.Sessions = make(map[string]string)
	}
	s.Sessions[key] = strings.TrimSpace(agent)
}

// findByPID returns the (key, agent) for a given PID in the state.
// Prefers the key with the highest start time (most specific match).
func (s *State) findByPID(pid int) (key, agent string) {
	if s == nil || s.Sessions == nil {
		return "", ""
	}
	var bestST int64
	for k, a := range s.Sessions {
		p, st, ok := parsePidKey(k)
		if !ok || p != pid {
			continue
		}
		if key == "" || st > bestST {
			key, agent, bestST = k, a, st
		}
	}
	return key, agent
}

// deleteByPID removes all session entries for the given PID.
func (s *State) deleteByPID(pid int) bool {
	deleted := false
	for k := range s.Sessions {
		p, _, ok := parsePidKey(k)
		if ok && p == pid {
			delete(s.Sessions, k)
			deleted = true
		}
	}
	return deleted
}

func cloneSessions(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// findEffectiveKey returns the nearest ancestor's stored key,
// or computes a fresh key for ppid if no ancestor match exists.
// Matches by parsed PID so lookups succeed even when processStartTime
// temporarily fails to reproduce the stored key exactly.
func findEffectiveKey(state *State, ppid int) string {
	for _, pid := range walkAncestors(ppid) {
		if key, _ := state.findByPID(pid); key != "" {
			return key
		}
	}
	return pidKey(ppid)
}

// GetCurrentAgent returns the agent for the current terminal session.
// Walks ancestor PIDs for pid:<PID> matches (nearest ancestor wins).
func GetCurrentAgent() string {
	ppid := os.Getppid()
	path, err := sessionsPath()
	if err != nil {
		return ""
	}
	var agent string
	_ = withLockedState(path, func(state *State) (bool, error) {
		for _, pid := range walkAncestors(ppid) {
			if _, a := state.findByPID(pid); a != "" {
				agent = a
				return false, nil
			}
		}
		return false, nil
	})
	return agent
}

// clearAncestorKeys deletes all session entries along the ancestor chain.
// Matches by parsed PID so clears succeed even when processStartTime
// cannot reproduce the stored key exactly.
func clearAncestorKeys(state *State, ppid int) bool {
	changed := false
	for _, pid := range walkAncestors(ppid) {
		if state.deleteByPID(pid) {
			changed = true
		}
	}
	return changed
}

// SetCurrentAgent sets the agent for the current terminal session.
// Resolves the effective key via ancestor walk so that nested shells
// update the same key that GetCurrentAgent would read.
// An empty agent clears all ancestor keys (same as ClearCurrentAgent).
func SetCurrentAgent(agent string) error {
	ppid := os.Getppid()
	if ppid <= 1 {
		return fmt.Errorf("no usable parent process (ppid=%d)", ppid)
	}
	path, err := sessionsPath()
	if err != nil {
		return err
	}
	agent = strings.TrimSpace(agent)
	return withLockedState(path, func(state *State) (bool, error) {
		if agent == "" {
			return clearAncestorKeys(state, ppid), nil
		}
		key := findEffectiveKey(state, ppid)
		before := state.Get(key)
		state.Set(key, agent)
		return before != agent, nil
	})
}

// ClearCurrentAgent clears the current terminal session's selected agent.
// Deletes all ancestor keys to prevent fallback resurfacing.
func ClearCurrentAgent() error {
	ppid := os.Getppid()
	if ppid <= 1 {
		return fmt.Errorf("no usable parent process (ppid=%d)", ppid)
	}
	path, err := sessionsPath()
	if err != nil {
		return err
	}
	return withLockedState(path, func(state *State) (bool, error) {
		return clearAncestorKeys(state, ppid), nil
	})
}
