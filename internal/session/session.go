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

func sessionsPath() (string, error) {
	runtimeDir, err := xdg.RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(runtimeDir, "sessions.json"), nil
}

// CurrentSID returns the session ID of the current terminal.
func CurrentSID() (int, error) {
	return unix.Getsid(0)
}

// CurrentKey returns the identity key for this terminal/session.
// Primary key shape: tty:<rdev>:ppid:<pid>
// Fallback key shape: sid:<sid>
func CurrentKey() (string, error) {
	ttyRdev, err := currentTTYDeviceID()
	if err == nil {
		return fmt.Sprintf("tty:%d:ppid:%d", ttyRdev, os.Getppid()), nil
	}

	sid, err := CurrentSID()
	if err != nil {
		return "", err
	}
	return sidKey(sid), nil
}

func currentTTYDeviceID() (uint64, error) {
	var st unix.Stat_t
	if err := unix.Stat("/dev/tty", &st); err == nil {
		return uint64(st.Rdev), nil
	}
	if err := unix.Fstat(0, &st); err == nil {
		return uint64(st.Rdev), nil
	}
	return 0, fmt.Errorf("unable to determine tty device")
}

func processExists(pid int) bool {
	// kill with signal 0 checks if process exists without sending signal
	err := unix.Kill(pid, 0)
	return err == nil
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

func sidKey(sid int) string {
	return fmt.Sprintf("sid:%d", sid)
}

func legacySIDKey(sid int) string {
	return strconv.Itoa(sid)
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

		switch {
		case strings.HasPrefix(key, "sid:"):
			sidStr := strings.TrimPrefix(key, "sid:")
			sid, err := strconv.Atoi(sidStr)
			if err != nil || !processExists(sid) {
				changed = true
				continue
			}
			converted[sidKey(sid)] = agent
		case isAllDigits(key):
			sid, err := strconv.Atoi(key)
			if err != nil || !processExists(sid) {
				changed = true
				continue
			}
			converted[sidKey(sid)] = agent
			changed = true
		case strings.HasPrefix(key, "tty:"):
			parts := strings.Split(key, ":")
			if len(parts) != 4 || parts[2] != "ppid" {
				changed = true
				continue
			}
			ppid, err := strconv.Atoi(parts[3])
			if err != nil || !processExists(ppid) {
				changed = true
				continue
			}
			converted[key] = agent
		default:
			changed = true
		}
	}
	if len(converted) != len(s.Sessions) {
		changed = true
	}
	s.Sessions = converted
	return changed
}

func isAllDigits(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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

// GetCurrentAgent returns the agent for the current terminal session.
func GetCurrentAgent() string {
	key, err := CurrentKey()
	if err != nil {
		return ""
	}
	sid, sidErr := CurrentSID()
	var agent string
	path, err := sessionsPath()
	if err != nil {
		return ""
	}
	err = withLockedState(path, func(state *State) (bool, error) {
		agent = state.Get(key)
		if agent != "" {
			return false, nil
		}
		// Backward compatibility for legacy numeric SID keys.
		if sidErr == nil {
			legacyKey := legacySIDKey(sid)
			legacyAgent := state.Get(legacyKey)
			if legacyAgent != "" {
				state.Set(key, legacyAgent)
				delete(state.Sessions, legacyKey)
				agent = legacyAgent
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return ""
	}
	return agent
}

// SetCurrentAgent sets the agent for the current terminal session.
func SetCurrentAgent(agent string) error {
	key, err := CurrentKey()
	if err != nil {
		return err
	}
	sid, sidErr := CurrentSID()
	path, err := sessionsPath()
	if err != nil {
		return err
	}
	agent = strings.TrimSpace(agent)
	return withLockedState(path, func(state *State) (bool, error) {
		before := state.Get(key)
		if agent == "" {
			delete(state.Sessions, key)
		} else {
			state.Set(key, agent)
		}
		if sidErr == nil {
			delete(state.Sessions, legacySIDKey(sid))
		}
		after := state.Get(key)
		return before != after, nil
	})
}

// ClearCurrentAgent clears the current terminal session's selected agent.
func ClearCurrentAgent() error {
	key, err := CurrentKey()
	if err != nil {
		return err
	}
	sid, sidErr := CurrentSID()
	path, err := sessionsPath()
	if err != nil {
		return err
	}
	return withLockedState(path, func(state *State) (bool, error) {
		changed := false
		if _, ok := state.Sessions[key]; ok {
			delete(state.Sessions, key)
			changed = true
		}
		if sidErr == nil {
			legacy := legacySIDKey(sid)
			if _, ok := state.Sessions[legacy]; ok {
				delete(state.Sessions, legacy)
				changed = true
			}
		}
		return changed, nil
	})
}
