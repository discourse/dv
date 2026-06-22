package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/xdg"
)

func TestRemoveRunsHooksAroundSuccessfulDockerRemoval(t *testing.T) {
	configDir := setupRemoveTestConfig(t, func(cfg *config.Config, orderPath string) {
		cfg.Hooks.PreRemove = []config.HostHook{{Command: fmt.Sprintf("printf 'pre:%%s:%%s:%%s:%%s\\n' \"$DV_HOOK\" \"$DV_IMAGE_TAG\" \"$DV_WORKDIR\" \"$DV_CONTAINER_PORT\" >> %s", shellQuote(orderPath))}}
		cfg.Hooks.PostRemove = []config.HostHook{{Command: fmt.Sprintf("printf 'post:%%s:%%s:%%s:%%s\\n' \"$DV_HOOK\" \"$DV_IMAGE_TAG\" \"$DV_WORKDIR\" \"$DV_CONTAINER_PORT\" >> %s", shellQuote(orderPath))}}
	})
	orderPath := filepath.Join(configDir, "order")

	restore := stubRemoveDocker(t)
	defer restore()
	removeDockerRemove = func(name string) error {
		appendRemoveTestLine(t, orderPath, "docker-rm:"+name)
		return nil
	}

	cmd, stdout, stderr := removeTestCommand()
	if err := removeCmd.RunE(cmd, []string{"agent-one"}); err != nil {
		t.Fatalf("remove RunE returned error: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Removal complete") {
		t.Fatalf("stdout = %q, want completion", stdout.String())
	}

	got := readRemoveTestFile(t, orderPath)
	want := "pre:preRemove:custom-tag:/workspace:9292\ndocker-rm:agent-one\npost:postRemove:custom-tag:/workspace:9292\n"
	if got != want {
		t.Fatalf("hook/removal order = %q, want %q", got, want)
	}
}

func TestRemovePreRemoveFailureAbortsDockerRemoval(t *testing.T) {
	configDir := setupRemoveTestConfig(t, func(cfg *config.Config, orderPath string) {
		cfg.Hooks.PreRemove = []config.HostHook{{Command: fmt.Sprintf("printf 'pre\\n' >> %s; exit 9", shellQuote(orderPath))}}
		cfg.Hooks.PostRemove = []config.HostHook{{Command: fmt.Sprintf("printf 'post\\n' >> %s", shellQuote(orderPath))}}
	})
	orderPath := filepath.Join(configDir, "order")

	restore := stubRemoveDocker(t)
	defer restore()
	removeDockerRemove = func(name string) error {
		t.Fatalf("docker remove should not run after preRemove failure")
		return nil
	}

	cmd, _, _ := removeTestCommand()
	err := removeCmd.RunE(cmd, []string{"agent-one"})
	if err == nil || !strings.Contains(err.Error(), "preRemove hook failed") {
		t.Fatalf("error = %v, want preRemove hook failure", err)
	}
	if got := readRemoveTestFile(t, orderPath); got != "pre\n" {
		t.Fatalf("order = %q, want only pre hook", got)
	}
}

func TestRemoveDockerFailureCleansConfigButSkipsPostRemove(t *testing.T) {
	configDir := setupRemoveTestConfig(t, func(cfg *config.Config, orderPath string) {
		cfg.Hooks.PreRemove = []config.HostHook{{Command: fmt.Sprintf("printf 'pre\\n' >> %s", shellQuote(orderPath))}}
		cfg.Hooks.PostRemove = []config.HostHook{{Command: fmt.Sprintf("printf 'post\\n' >> %s", shellQuote(orderPath))}}
	})
	orderPath := filepath.Join(configDir, "order")

	restore := stubRemoveDocker(t)
	defer restore()
	removeDockerRemove = func(name string) error {
		appendRemoveTestLine(t, orderPath, "docker-rm")
		return errors.New("boom")
	}

	cmd, _, _ := removeTestCommand()
	err := removeCmd.RunE(cmd, []string{"agent-one"})
	if err == nil || !strings.Contains(err.Error(), "remove container \"agent-one\": boom") {
		t.Fatalf("error = %v, want docker removal failure", err)
	}
	if got := readRemoveTestFile(t, orderPath); got != "pre\ndocker-rm\n" {
		t.Fatalf("order = %q, want pre and docker remove only", got)
	}

	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if _, ok := cfg.ContainerImages["agent-one"]; ok {
		t.Fatal("ContainerImages[agent-one] was not cleaned up")
	}
	if _, ok := cfg.LabelOverrides["agent-one"]; ok {
		t.Fatal("LabelOverrides[agent-one] was not cleaned up")
	}
	if _, ok := cfg.CustomWorkdirs["agent-one"]; ok {
		t.Fatal("CustomWorkdirs[agent-one] was not cleaned up")
	}
}

func TestRemoveSkipsHooksWhenContainerDoesNotExist(t *testing.T) {
	configDir := setupRemoveTestConfig(t, func(cfg *config.Config, orderPath string) {
		cfg.Hooks.PreRemove = []config.HostHook{{Command: fmt.Sprintf("printf 'pre\\n' >> %s", shellQuote(orderPath))}}
		cfg.Hooks.PostRemove = []config.HostHook{{Command: fmt.Sprintf("printf 'post\\n' >> %s", shellQuote(orderPath))}}
	})
	orderPath := filepath.Join(configDir, "order")

	restore := stubRemoveDocker(t)
	defer restore()
	removeDockerExists = func(name string) bool { return false }

	cmd, stdout, _ := removeTestCommand()
	if err := removeCmd.RunE(cmd, []string{"agent-one"}); err != nil {
		t.Fatalf("remove RunE returned error: %v", err)
	}
	if !strings.Contains(stdout.String(), "Container 'agent-one' does not exist") {
		t.Fatalf("stdout = %q, want missing container message", stdout.String())
	}
	if _, err := os.Stat(orderPath); !os.IsNotExist(err) {
		t.Fatalf("hooks should not create order file, stat err = %v", err)
	}
}

func setupRemoveTestConfig(t *testing.T, mutate func(*config.Config, string)) string {
	t.Helper()
	t.Setenv("DV_NO_HOOKS", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	configDir, err := xdg.ConfigDir()
	if err != nil {
		t.Fatalf("config dir: %v", err)
	}
	orderPath := filepath.Join(configDir, "order")

	cfg := config.Default()
	cfg.SelectedAgent = "other-agent"
	cfg.ImageTag = "custom-tag"
	cfg.ContainerPort = 9292
	cfg.Images["discourse"] = config.ImageConfig{Tag: "custom-tag", Workdir: "/workspace", ContainerPort: 9292}
	cfg.ContainerImages["agent-one"] = "discourse"
	cfg.LabelOverrides = map[string]map[string]string{"agent-one": {
		"com.dv.image-name": "discourse",
		"com.dv.image-tag":  "custom-tag",
	}}
	cfg.CustomWorkdirs["agent-one"] = "/workspace"
	mutate(&cfg, orderPath)
	if err := config.Save(configDir, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	return configDir
}

func removeTestCommand() (*cobra.Command, *strings.Builder, *strings.Builder) {
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	cmd := &cobra.Command{Use: "remove"}
	cmd.Flags().Bool("image", false, "")
	cmd.Flags().String("name", "", "")
	cmd.Flags().BoolP("force", "f", true, "")
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetIn(strings.NewReader(""))
	return cmd, stdout, stderr
}

func stubRemoveDocker(t *testing.T) func() {
	t.Helper()
	oldExists := removeDockerExists
	oldRunning := removeDockerRunning
	oldRemove := removeDockerRemove
	oldRemoveForce := removeDockerRemoveForce
	oldImageExists := removeDockerImageExists
	oldRemoveImage := removeDockerRemoveImage
	removeDockerExists = func(name string) bool { return true }
	removeDockerRunning = func(name string) bool { return false }
	removeDockerRemove = func(name string) error { return nil }
	removeDockerRemoveForce = func(name string) error { return nil }
	removeDockerImageExists = func(tag string) bool { return false }
	removeDockerRemoveImage = func(tag string) error { return nil }
	return func() {
		removeDockerExists = oldExists
		removeDockerRunning = oldRunning
		removeDockerRemove = oldRemove
		removeDockerRemoveForce = oldRemoveForce
		removeDockerImageExists = oldImageExists
		removeDockerRemoveImage = oldRemoveImage
	}
}

func appendRemoveTestLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open order file: %v", err)
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, line); err != nil {
		t.Fatalf("append order file: %v", err)
	}
}

func readRemoveTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
