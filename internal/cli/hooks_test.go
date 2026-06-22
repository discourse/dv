package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"dv/internal/config"
)

func TestRunConfiguredHostHooksPassesEnvironment(t *testing.T) {
	t.Setenv("DV_NO_HOOKS", "")
	t.Setenv("DV_VERBOSE", "")
	t.Setenv("DV_CONTAINER_NAME", "host-value-should-not-leak")

	tmp := t.TempDir()
	outPath := filepath.Join(tmp, "hook.out")
	cfg := config.Config{
		Hooks: config.HooksConfig{
			PostCreate: []config.HostHook{
				{
					Command: fmt.Sprintf("printf '%%s' \"$DV_HOOK|$DV_COMMAND|$DV_CONTAINER_NAME|$DV_IMAGE_NAME|$DV_IMAGE_TAG|$DV_WORKDIR|$DV_HOST_PORT|$DV_CONTAINER_PORT|$DV_CONFIG_DIR|$DV_DATA_DIR|$DV_HOOK_INDEX|$DV_NO_HOOKS\" > %s", shellQuote(outPath)),
				},
			},
		},
	}

	cmd, stdout, stderr := hookTestCommand("start")
	err := runConfiguredHostHooks(cmd, cfg, hostHookPostCreate, hostHookContext{
		CommandName:   "start",
		ContainerName: "agent-one",
		ImageName:     "discourse",
		ImageTag:      "ai_agent",
		Workdir:       "/var/www/discourse",
		HostPort:      3001,
		ContainerPort: 3000,
		ConfigDir:     "/tmp/dv-config",
		DataDir:       "/tmp/dv-data",
	})
	if err != nil {
		t.Fatalf("runConfiguredHostHooks returned error: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Running postCreate hook") {
		t.Fatalf("stdout = %q, want hook progress", stdout.String())
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read hook output: %v", err)
	}
	want := "postCreate|start|agent-one|discourse|ai_agent|/var/www/discourse|3001|3000|/tmp/dv-config|/tmp/dv-data|0|1"
	if string(got) != want {
		t.Fatalf("hook env output = %q, want %q", string(got), want)
	}
}

func TestRunConfiguredHostHooksPassesArgv(t *testing.T) {
	t.Setenv("DV_NO_HOOKS", "")

	tmp := t.TempDir()
	outPath := filepath.Join(tmp, "hook-argv.out")
	cfg := config.Config{
		Hooks: config.HooksConfig{
			PostStart: []config.HostHook{
				{
					Command: fmt.Sprintf("printf '%%s' \"$1|$2|$3|$4|$5|$6|$7\" > %s", shellQuote(outPath)),
				},
			},
		},
	}

	cmd, _, _ := hookTestCommand("start")
	err := runConfiguredHostHooks(cmd, cfg, hostHookPostStart, hostHookContext{
		ContainerName: "agent-two",
		ImageName:     "discourse",
		ImageTag:      "ai_agent",
		Workdir:       "/var/www/discourse",
		HostPort:      3002,
		ContainerPort: 3000,
	})
	if err != nil {
		t.Fatalf("runConfiguredHostHooks returned error: %v", err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read hook argv output: %v", err)
	}
	want := "agent-two|3002|3000|postStart|discourse|ai_agent|/var/www/discourse"
	if string(got) != want {
		t.Fatalf("hook argv output = %q, want %q", string(got), want)
	}
}

func TestRunConfiguredHostHooksSkipsWhenDisabled(t *testing.T) {
	t.Setenv("DV_NO_HOOKS", "")

	tmp := t.TempDir()
	outPath := filepath.Join(tmp, "disabled.out")
	enabled := false
	cfg := config.Config{
		Hooks: config.HooksConfig{
			PostStart: []config.HostHook{
				{Command: fmt.Sprintf("touch %s", shellQuote(outPath)), Enabled: &enabled},
			},
		},
	}

	cmd, _, _ := hookTestCommand("start")
	if err := runConfiguredHostHooks(cmd, cfg, hostHookPostStart, hostHookContext{}); err != nil {
		t.Fatalf("runConfiguredHostHooks returned error: %v", err)
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatalf("disabled hook created output file; stat err = %v", err)
	}
}

func TestRunConfiguredHostHooksSkipsWithDVNoHooks(t *testing.T) {
	t.Setenv("DV_NO_HOOKS", "1")

	cfg := config.Config{
		Hooks: config.HooksConfig{
			PostStart: []config.HostHook{{Command: "exit 42"}},
		},
	}

	cmd, stdout, stderr := hookTestCommand("start")
	if err := runConfiguredHostHooks(cmd, cfg, hostHookPostStart, hostHookContext{}); err != nil {
		t.Fatalf("runConfiguredHostHooks returned error with DV_NO_HOOKS=1: %v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("expected no output when hooks disabled, stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunConfiguredHostHooksHonorsIgnoreErrors(t *testing.T) {
	t.Setenv("DV_NO_HOOKS", "")

	cfg := config.Config{
		Hooks: config.HooksConfig{
			PostStart: []config.HostHook{{Command: "exit 7", IgnoreErrors: true}},
		},
	}

	cmd, _, stderr := hookTestCommand("start")
	if err := runConfiguredHostHooks(cmd, cfg, hostHookPostStart, hostHookContext{}); err != nil {
		t.Fatalf("runConfiguredHostHooks returned error: %v", err)
	}
	if !strings.Contains(stderr.String(), "Warning: postStart hook failed") {
		t.Fatalf("stderr = %q, want warning", stderr.String())
	}
}

func TestRunConfiguredHostHooksFailsByDefault(t *testing.T) {
	t.Setenv("DV_NO_HOOKS", "")

	cfg := config.Config{
		Hooks: config.HooksConfig{
			PostStart: []config.HostHook{{Command: "exit 7"}},
		},
	}

	cmd, _, _ := hookTestCommand("start")
	err := runConfiguredHostHooks(cmd, cfg, hostHookPostStart, hostHookContext{})
	if err == nil {
		t.Fatal("expected hook failure error")
	}
	if !strings.Contains(err.Error(), "postStart hook failed") {
		t.Fatalf("error = %v, want postStart hook failed", err)
	}
}

func hookTestCommand(name string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd := &cobra.Command{Use: name}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetIn(strings.NewReader(""))
	return cmd, stdout, stderr
}

func TestRunConfiguredHostHooksUsesExecutionIndex(t *testing.T) {
	t.Setenv("DV_NO_HOOKS", "")

	tmp := t.TempDir()
	outPath := filepath.Join(tmp, "hook-order.out")
	enabled := false
	appendIndex := func() string {
		return fmt.Sprintf("printf '%%s\\n' \"$DV_HOOK_INDEX\" >> %s", shellQuote(outPath))
	}
	cfg := config.Config{
		Hooks: config.HooksConfig{
			PostStart: []config.HostHook{
				{Command: "", IgnoreErrors: true},
				{Command: appendIndex(), Enabled: &enabled},
				{Command: appendIndex()},
				{Command: appendIndex()},
			},
		},
	}

	cmd, stdout, _ := hookTestCommand("start")
	if err := runConfiguredHostHooks(cmd, cfg, hostHookPostStart, hostHookContext{}); err != nil {
		t.Fatalf("runConfiguredHostHooks returned error: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "Running postStart hook 1/2") || !strings.Contains(got, "Running postStart hook 2/2") {
		t.Fatalf("stdout = %q, want hook progress with runnable counts", got)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read hook order output: %v", err)
	}
	if string(got) != "0\n1\n" {
		t.Fatalf("hook indexes = %q, want 0 and 1", string(got))
	}
}

func TestRunConfiguredHostHooksSupportsPreRemove(t *testing.T) {
	t.Setenv("DV_NO_HOOKS", "")

	tmp := t.TempDir()
	outPath := filepath.Join(tmp, "pre-remove.out")
	cfg := config.Config{
		Hooks: config.HooksConfig{
			PreRemove: []config.HostHook{{Command: fmt.Sprintf("printf '%%s' \"$DV_HOOK|$DV_CONTAINER_NAME|$DV_IMAGE_NAME|$DV_NO_HOOKS\" > %s", shellQuote(outPath))}},
		},
	}

	cmd, stdout, stderr := hookTestCommand("remove")
	err := runConfiguredHostHooks(cmd, cfg, hostHookPreRemove, hostHookContext{
		ContainerName: "amazing-feature",
		ImageName:     "discourse",
	})
	if err != nil {
		t.Fatalf("runConfiguredHostHooks returned error: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Running preRemove hook") {
		t.Fatalf("stdout = %q, want preRemove progress", stdout.String())
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read preRemove output: %v", err)
	}
	want := "preRemove|amazing-feature|discourse|1"
	if string(got) != want {
		t.Fatalf("preRemove output = %q, want %q", string(got), want)
	}
}

func TestRunConfiguredHostHooksSupportsPostRemove(t *testing.T) {
	t.Setenv("DV_NO_HOOKS", "")

	tmp := t.TempDir()
	outPath := filepath.Join(tmp, "post-remove.out")
	cfg := config.Config{
		Hooks: config.HooksConfig{
			PostRemove: []config.HostHook{{Command: fmt.Sprintf("printf '%%s' \"$DV_HOOK|$DV_CONTAINER_NAME|$DV_IMAGE_NAME|$DV_NO_HOOKS\" > %s", shellQuote(outPath))}},
		},
	}

	cmd, stdout, stderr := hookTestCommand("remove")
	err := runConfiguredHostHooks(cmd, cfg, hostHookPostRemove, hostHookContext{
		ContainerName: "amazing-feature",
		ImageName:     "discourse",
	})
	if err != nil {
		t.Fatalf("runConfiguredHostHooks returned error: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Running postRemove hook") {
		t.Fatalf("stdout = %q, want postRemove progress", stdout.String())
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read postRemove output: %v", err)
	}
	want := "postRemove|amazing-feature|discourse|1"
	if string(got) != want {
		t.Fatalf("postRemove output = %q, want %q", string(got), want)
	}
}
