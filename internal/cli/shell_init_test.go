package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestShellActionsAreDataOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(shellActionDirEnv, dir)

	requestShellAgent("agent with spaces")
	requestShellChdir("/tmp/work tree")

	assertShellActionFile(t, filepath.Join(dir, shellActionAgent), "agent with spaces")
	assertShellActionFile(t, filepath.Join(dir, shellActionChdir), "/tmp/work tree")
	if _, err := os.Stat(filepath.Join(dir, shellActionAgentUnset)); !os.IsNotExist(err) {
		t.Fatalf("agent-unset should not exist after setting an agent: %v", err)
	}

	requestShellAgent("")
	assertShellActionFile(t, filepath.Join(dir, shellActionAgentUnset), "")
	if _, err := os.Stat(filepath.Join(dir, shellActionAgent)); !os.IsNotExist(err) {
		t.Fatalf("agent should be removed after unsetting it: %v", err)
	}
}

func TestShellQuoting(t *testing.T) {
	if got, want := quotePOSIXShell("agent's workspace"), `'agent'"'"'s workspace'`; got != want {
		t.Fatalf("quotePOSIXShell = %q, want %q", got, want)
	}
	if got, want := quoteFishShell(`agent's \\ workspace`), `'agent\'s \\\\ workspace'`; got != want {
		t.Fatalf("quoteFishShell = %q, want %q", got, want)
	}
}

func TestSelectRequestsShellAgentUpdate(t *testing.T) {
	actionDir := t.TempDir()
	t.Setenv(shellActionDirEnv, actionDir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DV_AGENT", "")

	cmd := &cobra.Command{Use: "select"}
	cmd.SetOut(&bytes.Buffer{})
	if err := selectCmd.RunE(cmd, []string{"selected-agent"}); err != nil {
		t.Fatalf("select agent: %v", err)
	}
	assertShellActionFile(t, filepath.Join(actionDir, shellActionAgent), "selected-agent")
}

func TestGenerateShellInitIncludesCompletionAndWrapper(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			var output bytes.Buffer
			if err := generateShellInit(&output, shell, "agent-one"); err != nil {
				t.Fatalf("generateShellInit(%q): %v", shell, err)
			}
			got := output.String()
			for _, want := range []string{"DV_AGENT", "DV_SHELL_ACTION_DIR"} {
				if !strings.Contains(got, want) {
					t.Fatalf("%s integration missing %q", shell, want)
				}
			}
			wrapperMarker := "dv() {"
			if shell == "fish" {
				wrapperMarker = "function dv"
			}
			if !strings.Contains(got, wrapperMarker) {
				t.Fatalf("%s integration missing wrapper %q", shell, wrapperMarker)
			}
			if shell == "fish" {
				if !strings.Contains(got, "complete -c dv") {
					t.Fatal("fish integration missing generated completions")
				}
			} else if !strings.Contains(got, "__complete") {
				t.Fatalf("%s integration missing generated completions", shell)
			}
		})
	}
}

func TestPOSIXShellWrapperAppliesAgentAndChdirActions(t *testing.T) {
	for _, tc := range []struct {
		name string
		bin  string
		args []string
	}{
		{name: "bash", bin: "bash", args: []string{"--noprofile", "--norc"}},
		{name: "zsh", bin: "zsh", args: []string{"-f"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shellPath, err := exec.LookPath(tc.bin)
			if err != nil {
				t.Skipf("%s is not installed", tc.bin)
			}

			binDir := t.TempDir()
			fakeDV := filepath.Join(binDir, "dv")
			fakeScript := `#!/bin/sh
case "$1" in
  select) printf '%s' "$2" > "$DV_SHELL_ACTION_DIR/agent" ;;
  extract) mkdir -p "$TEST_DEST"; printf '%s' "$TEST_DEST" > "$DV_SHELL_ACTION_DIR/chdir" ;;
  fail) printf '%s' "$TEST_DEST" > "$DV_SHELL_ACTION_DIR/chdir"; exit 9 ;;
esac
`
			if err := os.WriteFile(fakeDV, []byte(fakeScript), 0o755); err != nil {
				t.Fatalf("write fake dv: %v", err)
			}

			var integration bytes.Buffer
			integration.WriteString("alias dv='printf alias-was-used\\n'\n")
			writePOSIXShellWrapper(&integration, "initial-agent")
			script := integration.String() + `
dv_shell_post_command() {
  printf 'hook=%s:%s\n' "$1" "$2"
}
set -e
printf 'initial=%s\n' "$DV_AGENT"
dv select selected-agent
printf 'selected=%s\n' "$DV_AGENT"
cd -- "$TEST_START"
dv extract
printf 'cwd=%s\n' "$PWD"
set +e
cd -- "$TEST_START"
dv fail
printf 'failure-status=%s cwd=%s\n' "$?" "$PWD"
`
			scriptPath := filepath.Join(t.TempDir(), "integration.sh")
			if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
				t.Fatalf("write integration script: %v", err)
			}

			startDir := t.TempDir()
			runtimeDir := t.TempDir()
			destination := filepath.Join(t.TempDir(), "destination with spaces")
			args := append(append([]string{}, tc.args...), scriptPath)
			cmd := exec.Command(shellPath, args...)
			cmd.Env = append(environmentWithout(os.Environ(), "DV_AGENT", "XDG_RUNTIME_DIR"),
				"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"XDG_RUNTIME_DIR="+runtimeDir,
				"TEST_START="+startDir,
				"TEST_DEST="+destination,
			)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("run %s integration: %v\n%s", tc.name, err, output)
			}
			got := string(output)
			for _, want := range []string{
				"initial=initial-agent",
				"selected=selected-agent",
				"hook=0:select",
				"hook=0:extract",
				"hook=9:fail",
				"cwd=" + destination,
				fmt.Sprintf("failure-status=9 cwd=%s", startDir),
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("%s output missing %q:\n%s", tc.name, want, got)
				}
			}
			if strings.Contains(got, "alias-was-used") {
				t.Fatalf("%s integration did not replace the existing dv alias:\n%s", tc.name, got)
			}
			if entries, err := os.ReadDir(runtimeDir); err != nil {
				t.Fatalf("read runtime directory: %v", err)
			} else if len(entries) != 0 {
				t.Fatalf("%s integration left action directories behind: %v", tc.name, entries)
			}
		})
	}
}

func TestFishShellWrapperAppliesAgentAndChdirActions(t *testing.T) {
	fishPath, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish is not installed")
	}

	binDir := t.TempDir()
	fakeDV := filepath.Join(binDir, "dv")
	fakeScript := `#!/bin/sh
case "$1" in
  select) printf '%s\n' "$2" > "$DV_SHELL_ACTION_DIR/agent" ;;
  unset) printf '\n' > "$DV_SHELL_ACTION_DIR/agent-unset" ;;
  extract) mkdir -p "$TEST_DEST"; printf '%s\n' "$TEST_DEST" > "$DV_SHELL_ACTION_DIR/chdir" ;;
  fail) printf '%s\n' "$TEST_DEST" > "$DV_SHELL_ACTION_DIR/chdir"; exit 9 ;;
esac
`
	if err := os.WriteFile(fakeDV, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	var integration bytes.Buffer
	writeFishShellWrapper(&integration, "initial-agent")
	script := integration.String() + `
function dv_shell_post_command
    printf 'hook=%s:%s\n' $argv[1] $argv[2]
end
printf 'initial=%s\n' "$DV_AGENT"
dv select selected-agent
printf 'selected=%s\n' "$DV_AGENT"
dv unset
if set -q DV_AGENT
    printf 'unset=no\n'
else
    printf 'unset=yes\n'
end
cd -- "$TEST_START"
dv extract
printf 'cwd=%s\n' "$PWD"
cd -- "$TEST_START"
dv fail
set -l failure_status $status
printf 'failure-status=%s cwd=%s\n' $failure_status "$PWD"
`
	scriptPath := filepath.Join(t.TempDir(), "integration.fish")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}

	startDir := t.TempDir()
	runtimeDir := t.TempDir()
	destination := filepath.Join(t.TempDir(), "destination with spaces")
	cmd := exec.Command(fishPath, "--no-config", scriptPath)
	cmd.Env = append(environmentWithout(os.Environ(), "DV_AGENT", "XDG_RUNTIME_DIR"),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_RUNTIME_DIR="+runtimeDir,
		"TEST_START="+startDir,
		"TEST_DEST="+destination,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run fish integration: %v\n%s", err, output)
	}
	got := string(output)
	for _, want := range []string{
		"initial=initial-agent",
		"selected=selected-agent",
		"unset=yes",
		"hook=0:select",
		"hook=0:extract",
		"hook=9:fail",
		"cwd=" + destination,
		fmt.Sprintf("failure-status=9 cwd=%s", startDir),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("fish output missing %q:\n%s", want, got)
		}
	}
	if entries, err := os.ReadDir(runtimeDir); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("fish integration left action directories behind: %v", entries)
	}
}

func TestInstallZshShellInitUsesOneSourceLineAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	dataHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", "")
	t.Setenv("XDG_DATA_HOME", dataHome)

	zshrcPath := filepath.Join(home, ".zshrc")
	legacy := "export EDITOR=vim\n" + `eval "$(command dv config shell-init zsh)"` + "\n"
	if err := os.WriteFile(zshrcPath, []byte(legacy), 0o640); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := installZshShellInit(&output); err != nil {
		t.Fatalf("installZshShellInit: %v", err)
	}

	integrationPath := filepath.Join(dataHome, "dv", "shell-init.zsh")
	integration, err := os.ReadFile(integrationPath)
	if err != nil {
		t.Fatalf("read installed integration: %v", err)
	}
	if !strings.Contains(string(integration), "dv() {") || !strings.Contains(string(integration), "__complete") {
		t.Fatal("installed integration is missing wrapper or completions")
	}
	if strings.Contains(string(integration), "initial-agent") {
		t.Fatal("installed integration must not contain a baked agent selection")
	}

	wantZshrc := "export EDITOR=vim\n" + zshShellInitSourceLine + "\n"
	gotZshrc, err := os.ReadFile(zshrcPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotZshrc) != wantZshrc {
		t.Fatalf(".zshrc = %q, want %q", gotZshrc, wantZshrc)
	}
	if info, err := os.Stat(zshrcPath); err != nil {
		t.Fatal(err)
	} else if got, want := info.Mode().Perm(), os.FileMode(0o640); got != want {
		t.Fatalf(".zshrc mode = %o, want %o", got, want)
	}

	output.Reset()
	if err := installZshShellInit(&output); err != nil {
		t.Fatalf("second installZshShellInit: %v", err)
	}
	gotZshrc, err = os.ReadFile(zshrcPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotZshrc) != wantZshrc {
		t.Fatalf(".zshrc after second install = %q, want %q", gotZshrc, wantZshrc)
	}
	if !strings.Contains(output.String(), "already configured") {
		t.Fatalf("second install output = %q", output.String())
	}
}

func TestInstallZshShellInitRespectsZDOTDIR(t *testing.T) {
	zdotdir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZDOTDIR", zdotdir)
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	if err := installZshShellInit(&bytes.Buffer{}); err != nil {
		t.Fatalf("installZshShellInit: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(zdotdir, ".zshrc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != zshShellInitSourceLine+"\n" {
		t.Fatalf("ZDOTDIR .zshrc = %q", content)
	}
}

func TestEnsureZshShellInitSourcePreservesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "zshrc")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("export PAGER=less\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".zshrc")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	changed, err := ensureZshShellInitSource(link)
	if err != nil {
		t.Fatalf("ensureZshShellInitSource: %v", err)
	}
	if !changed {
		t.Fatal("expected symlinked startup file to change")
	}
	if info, err := os.Lstat(link); err != nil {
		t.Fatal(err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal(".zshrc symlink was replaced")
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(content), "export PAGER=less\n"+zshShellInitSourceLine+"\n"; got != want {
		t.Fatalf("symlink target = %q, want %q", got, want)
	}
}

func TestGenerateShellInitRejectsUnknownShell(t *testing.T) {
	if err := generateShellInit(&bytes.Buffer{}, "powershell", ""); err == nil {
		t.Fatal("expected unsupported shell error")
	}
}

func assertShellActionFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want+"\n" {
		t.Fatalf("%s = %q, want %q", path, got, want+"\n")
	}
}
