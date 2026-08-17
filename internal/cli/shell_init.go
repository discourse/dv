package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/xdg"
)

var shellInitInstall bool

var shellInitCmd = &cobra.Command{
	Use:   "shell-init SHELL",
	Short: "Generate dynamic shell integration",
	Long: `Generate shell code that adds completions, keeps DV_AGENT synchronized with
this terminal, and changes into host workspaces after successful extraction.

Evaluate the output from your shell startup file. Supported shells are bash,
zsh, and fish. Use --install with zsh to install a cached integration and add
one source line to .zshrc.`,
	Args:      cobra.ExactArgs(1),
	ValidArgs: []string{"bash", "zsh", "fish"},
	RunE: func(cmd *cobra.Command, args []string) error {
		shell := strings.ToLower(args[0])
		if shellInitInstall {
			if shell != "zsh" {
				return fmt.Errorf("--install is currently supported only for zsh")
			}
			return installZshShellInit(cmd.OutOrStdout())
		}

		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}
		return generateShellInit(cmd.OutOrStdout(), shell, currentAgentName(cfg))
	},
}

func init() {
	shellInitCmd.Flags().BoolVar(&shellInitInstall, "install", false, "Install zsh integration and update .zshrc")
	configCmd.AddCommand(shellInitCmd)
}

const zshShellInitSourceLine = `source "${XDG_DATA_HOME:-$HOME/.local/share}/dv/shell-init.zsh"`

var legacyZshShellInitLines = map[string]bool{
	`eval "$(command dv config shell-init zsh)"`: true,
	`eval "$(dv config shell-init zsh)"`:         true,
}

func installZshShellInit(out io.Writer) error {
	dataDir, err := xdg.DataDir()
	if err != nil {
		return err
	}
	integrationPath := filepath.Join(dataDir, "shell-init.zsh")

	var integration bytes.Buffer
	// An installed script must not bake in the selection from installation time.
	// Commands still resolve the persisted selection, and selection-changing
	// commands update DV_AGENT in the current integrated shell.
	if err := generateShellInit(&integration, "zsh", ""); err != nil {
		return err
	}
	if err := writeFileAtomic(integrationPath, integration.Bytes(), 0o644); err != nil {
		return fmt.Errorf("install zsh integration: %w", err)
	}

	zshrcPath, err := zshStartupFile()
	if err != nil {
		return err
	}
	changed, err := ensureZshShellInitSource(zshrcPath)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Installed zsh integration to %s\n", integrationPath)
	if changed {
		fmt.Fprintf(out, "Added shell integration to %s\n", zshrcPath)
	} else {
		fmt.Fprintf(out, "Shell integration is already configured in %s\n", zshrcPath)
	}
	fmt.Fprintln(out, "Restart your shell or source the startup file to activate it.")
	return nil
}

func zshStartupFile() (string, error) {
	if zdotdir := os.Getenv("ZDOTDIR"); zdotdir != "" {
		return filepath.Join(zdotdir, ".zshrc"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".zshrc"), nil
}

func ensureZshShellInitSource(path string) (bool, error) {
	writePath := path
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return false, fmt.Errorf("resolve %s: %w", path, err)
		}
		writePath = resolved
	} else if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}

	content, err := os.ReadFile(writePath)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	lines := strings.Split(string(content), "\n")
	newLines := make([]string, 0, len(lines)+1)
	found := false
	changed := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		isCurrent := trimmed == zshShellInitSourceLine
		isLegacy := legacyZshShellInitLines[trimmed]
		if !isCurrent && !isLegacy {
			newLines = append(newLines, line)
			continue
		}
		if found {
			changed = true
			continue
		}
		found = true
		newLines = append(newLines, zshShellInitSourceLine)
		if line != zshShellInitSourceLine || isLegacy {
			changed = true
		}
	}
	if !found {
		if len(newLines) > 0 && newLines[len(newLines)-1] == "" {
			newLines[len(newLines)-1] = zshShellInitSourceLine
			newLines = append(newLines, "")
		} else {
			newLines = append(newLines, zshShellInitSourceLine)
		}
		changed = true
	}

	updated := strings.Join(newLines, "\n")
	if !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}
	if !changed && updated == string(content) {
		return false, nil
	}

	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(writePath); statErr == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(statErr) {
		return false, fmt.Errorf("stat %s: %w", path, statErr)
	}
	if err := writeFileAtomic(writePath, []byte(updated), mode); err != nil {
		return false, fmt.Errorf("update %s: %w", path, err)
	}
	return true, nil
}

func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func generateShellInit(out io.Writer, shell, initialAgent string) error {
	var completion bytes.Buffer
	switch shell {
	case "zsh":
		if err := rootCmd.GenZshCompletion(&completion); err != nil {
			return err
		}
		fmt.Fprintln(out, `if (( ! $+functions[compdef] )); then autoload -U compinit && compinit; fi`)
		if _, err := io.Copy(out, &completion); err != nil {
			return err
		}
		writePOSIXShellWrapper(out, initialAgent)
	case "bash":
		if err := rootCmd.GenBashCompletionV2(&completion, true); err != nil {
			return err
		}
		if _, err := io.Copy(out, &completion); err != nil {
			return err
		}
		writePOSIXShellWrapper(out, initialAgent)
	case "fish":
		if err := rootCmd.GenFishCompletion(&completion, true); err != nil {
			return err
		}
		if _, err := io.Copy(out, &completion); err != nil {
			return err
		}
		writeFishShellWrapper(out, initialAgent)
	default:
		return fmt.Errorf("unsupported shell %q (supported: bash, zsh, fish)", shell)
	}
	return nil
}

func writePOSIXShellWrapper(out io.Writer, initialAgent string) {
	if initialAgent != "" {
		fmt.Fprintf(out, "\nif [[ -z \"${DV_AGENT+x}\" ]]; then export DV_AGENT=%s; fi\n", quotePOSIXShell(initialAgent))
	}
	_, _ = io.WriteString(out, `
unalias dv 2>/dev/null || :
dv() {
  case "${1-}" in
    __complete*) command dv "$@"; return $? ;;
  esac

  local _dv_action_dir _dv_status _dv_tmp_root _dv_value
  _dv_tmp_root="${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}}"
  _dv_action_dir="$(mktemp -d "$_dv_tmp_root/dv-shell.XXXXXXXX")" || {
    command dv "$@"
    return $?
  }

  DV_SHELL_ACTION_DIR="$_dv_action_dir" command dv "$@"
  _dv_status=$?

  if [[ -f "$_dv_action_dir/agent-unset" ]]; then
    unset DV_AGENT
  elif [[ -f "$_dv_action_dir/agent" ]]; then
    IFS= read -r _dv_value < "$_dv_action_dir/agent" || :
    export DV_AGENT="$_dv_value"
  fi

  if [[ $_dv_status -eq 0 && -f "$_dv_action_dir/chdir" ]]; then
    IFS= read -r _dv_value < "$_dv_action_dir/chdir" || :
    if [[ -d "$_dv_value" ]]; then
      builtin cd -- "$_dv_value" || _dv_status=$?
    else
      printf 'dv: extracted directory no longer exists: %s\n' "$_dv_value" >&2
      _dv_status=1
    fi
  fi

  command rm -rf -- "$_dv_action_dir"

  if typeset -f dv_shell_post_command >/dev/null 2>&1; then
    dv_shell_post_command "$_dv_status" "$@" || :
  fi
  return $_dv_status
}
`)
}

func writeFishShellWrapper(out io.Writer, initialAgent string) {
	if initialAgent != "" {
		fmt.Fprintf(out, "\nif not set -q DV_AGENT; set -gx DV_AGENT %s; end\n", quoteFishShell(initialAgent))
	}
	_, _ = io.WriteString(out, `
function dv
    if test (count $argv) -gt 0; and string match -q '__complete*' -- "$argv[1]"
        command dv $argv
        return $status
    end

    set -l _dv_tmp_root /tmp
    if set -q XDG_RUNTIME_DIR; and test -n "$XDG_RUNTIME_DIR"
        set _dv_tmp_root "$XDG_RUNTIME_DIR"
    else if set -q TMPDIR; and test -n "$TMPDIR"
        set _dv_tmp_root "$TMPDIR"
    end
    set -l _dv_action_dir (mktemp -d "$_dv_tmp_root/dv-shell.XXXXXXXX")
    if test $status -ne 0; or test -z "$_dv_action_dir"
        command dv $argv
        return $status
    end

    command env DV_SHELL_ACTION_DIR="$_dv_action_dir" dv $argv
    set -l _dv_status $status

    if test -f "$_dv_action_dir/agent-unset"
        set -e DV_AGENT
    else if test -f "$_dv_action_dir/agent"
        read -l _dv_value < "$_dv_action_dir/agent"
        set -gx DV_AGENT "$_dv_value"
    end

    if test $_dv_status -eq 0; and test -f "$_dv_action_dir/chdir"
        read -l _dv_value < "$_dv_action_dir/chdir"
        if test -d "$_dv_value"
            builtin cd -- "$_dv_value"
            or set _dv_status $status
        else
            printf 'dv: extracted directory no longer exists: %s\n' "$_dv_value" >&2
            set _dv_status 1
        end
    end

    command rm -rf -- "$_dv_action_dir"

    if functions -q dv_shell_post_command
        dv_shell_post_command $_dv_status $argv
        or true
    end
    return $_dv_status
end
`)
}

func quotePOSIXShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func quoteFishShell(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `'`, `\'`)
	return "'" + value + "'"
}
