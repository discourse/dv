package cli

import (
	"os"
	"path/filepath"
	"strings"
)

const shellActionDirEnv = "DV_SHELL_ACTION_DIR"

const (
	shellActionChdir      = "chdir"
	shellActionAgent      = "agent"
	shellActionAgentUnset = "agent-unset"
)

// writeShellAction sends a data-only action to an installed shell wrapper.
// Commands behave normally when shell integration is not active.
func writeShellAction(name, value string) {
	dir := os.Getenv(shellActionDirEnv)
	if dir == "" {
		return
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o600)
}

func environmentWithout(env []string, keys ...string) []string {
	blocked := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		blocked[key] = struct{}{}
	}
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, remove := blocked[key]; remove {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func requestShellChdir(dir string) {
	writeShellAction(shellActionChdir, dir)
}

func requestShellAgent(agent string) {
	dir := os.Getenv(shellActionDirEnv)
	if dir == "" {
		return
	}
	if agent == "" {
		writeShellAction(shellActionAgentUnset, "")
		_ = os.Remove(filepath.Join(dir, shellActionAgent))
		return
	}
	writeShellAction(shellActionAgent, agent)
	_ = os.Remove(filepath.Join(dir, shellActionAgentUnset))
}
