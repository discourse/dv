package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/session"
	"dv/internal/xdg"
)

func TestRenameEnvOverrideDoesNotChangeDifferentSessionSelection(t *testing.T) {
	setupRenameSelectionTest(t, "global-agent", "session-agent", "old-agent")
	runRenameWithDockerStub(t, "old-agent", "new-agent")

	if got := session.GetCurrentAgent(); got != "session-agent" {
		t.Fatalf("session selection = %q, want session-agent", got)
	}
	assertShellActionFile(t, filepath.Join(os.Getenv(shellActionDirEnv), shellActionAgent), "new-agent")
}

func TestRenameUpdatesMatchingSessionSelection(t *testing.T) {
	setupRenameSelectionTest(t, "old-agent", "old-agent", "")
	runRenameWithDockerStub(t, "old-agent", "new-agent")

	if got := session.GetCurrentAgent(); got != "new-agent" {
		t.Fatalf("session selection = %q, want new-agent", got)
	}
	for _, action := range []string{shellActionAgent, shellActionAgentUnset} {
		if _, err := os.Stat(filepath.Join(os.Getenv(shellActionDirEnv), action)); !os.IsNotExist(err) {
			t.Fatalf("unexpected shell action %q: %v", action, err)
		}
	}
	configDir, err := xdg.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultContainer != "new-agent" {
		t.Fatalf("default container = %q, want new-agent", cfg.DefaultContainer)
	}
}

func setupRenameSelectionTest(t *testing.T, globalAgent, sessionAgent, envAgent string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DV_AGENT", envAgent)
	t.Setenv(shellActionDirEnv, t.TempDir())

	configDir, err := xdg.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.SelectedAgent = globalAgent
	cfg.DefaultContainer = globalAgent
	cfg.ContainerImages["old-agent"] = cfg.SelectedImage
	if err := config.Save(configDir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := session.SetCurrentAgent(sessionAgent); err != nil {
		t.Fatalf("set session agent: %v", err)
	}
}

func runRenameWithDockerStub(t *testing.T, oldName, newName string) {
	t.Helper()
	previousExists := renameDockerExists
	previousRename := renameDockerRenameContext
	renameDockerExists = func(name string) bool { return name == oldName }
	renameDockerRenameContext = func(context.Context, string, string, io.Writer, io.Writer) error { return nil }
	t.Cleanup(func() {
		renameDockerExists = previousExists
		renameDockerRenameContext = previousRename
	})

	cmd := &cobra.Command{Use: "rename"}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := runRename(cmd, oldName, newName); err != nil {
		t.Fatalf("runRename: %v", err)
	}
}
