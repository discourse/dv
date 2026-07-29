package cli

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"dv/internal/docker"
)

func TestInspectBranchWorktree(t *testing.T) {
	t.Parallel()

	var calls [][]string
	exec := func(name, workdir string, envs docker.Envs, argv []string) (string, string, error) {
		if name != "hades" || workdir != "/workspace" {
			t.Fatalf("unexpected target: %s %s", name, workdir)
		}
		calls = append(calls, append([]string(nil), argv...))
		switch len(calls) {
		case 1:
			// Successful stderr warnings must not contaminate parsed stdout.
			return "feature/safe-status\n", "warning: ignored config entry\n", nil
		case 2:
			return " M app/models/example.rb\n?? new-file.rb\n", "warning: ignored config entry\n", nil
		default:
			t.Fatalf("unexpected command: %v", argv)
			return "", "", nil
		}
	}

	status, err := inspectBranchWorktree("hades", "/workspace", exec)
	if err != nil {
		t.Fatal(err)
	}
	if status.branch != "feature/safe-status" {
		t.Fatalf("unexpected branch %q", status.branch)
	}
	if status.changes != " M app/models/example.rb\n?? new-file.rb" {
		t.Fatalf("unexpected changes %q", status.changes)
	}
	wantStatusArgs := []string{"git", "--no-optional-locks", "status", "--porcelain=v1", "--untracked-files=normal"}
	if !reflect.DeepEqual(calls[1], wantStatusArgs) {
		t.Fatalf("status args = %v, want %v", calls[1], wantStatusArgs)
	}
}

func TestInspectBranchWorktreeDetachedHead(t *testing.T) {
	t.Parallel()

	outputs := []string{"HEAD\n", "abc1234\n", ""}
	var calls [][]string
	exec := func(name, workdir string, envs docker.Envs, argv []string) (string, string, error) {
		calls = append(calls, append([]string(nil), argv...))
		if len(outputs) == 0 {
			t.Fatalf("unexpected command: %v", argv)
		}
		output := outputs[0]
		outputs = outputs[1:]
		return output, "", nil
	}

	status, err := inspectBranchWorktree("hades", "/workspace", exec)
	if err != nil {
		t.Fatal(err)
	}
	if status.branch != "detached at abc1234" {
		t.Fatalf("unexpected detached branch label %q", status.branch)
	}
	if status.changes != "" {
		t.Fatalf("expected clean worktree, got %q", status.changes)
	}
	wantDetachedArgs := []string{"git", "rev-parse", "--short", "HEAD"}
	if !reflect.DeepEqual(calls[1], wantDetachedArgs) {
		t.Fatalf("detached HEAD args = %v, want %v", calls[1], wantDetachedArgs)
	}
}

func TestInspectBranchWorktreeIncludesGitDiagnostics(t *testing.T) {
	t.Parallel()

	execErr := errors.New("exit status 128")
	exec := func(name, workdir string, envs docker.Envs, argv []string) (string, string, error) {
		return "", "fatal: not a git repository\n", execErr
	}

	_, err := inspectBranchWorktree("hades", "/workspace", exec)
	if err == nil {
		t.Fatal("expected git error")
	}
	if !errors.Is(err, execErr) {
		t.Fatalf("expected wrapped exec error, got %v", err)
	}
	for _, want := range []string{"hades", "/workspace", "fatal: not a git repository"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}

func TestBranchStatusRefusesStoppedContainerWithoutInspecting(t *testing.T) {
	t.Parallel()

	inspected := false
	cmd := newBranchStatusCommand(branchStatusDependencies{
		resolve: func(cmd *cobra.Command, overrideName ...string) (resolvedContainerTarget, error) {
			return resolvedContainerTarget{name: "sleeping", effectiveWorkdir: "/workspace"}, nil
		},
		exists:  func(string) bool { return true },
		running: func(string) bool { return false },
		inspect: func(containerName, workdir string, exec branchStatusExecFunc) (branchWorktreeStatus, error) {
			inspected = true
			return branchWorktreeStatus{}, nil
		},
		exec: func(name, workdir string, envs docker.Envs, argv []string) (string, string, error) {
			t.Fatal("git execution must not run for a stopped container")
			return "", "", nil
		},
	})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "is not running") {
		t.Fatalf("expected stopped-container error, got %v", err)
	}
	if inspected {
		t.Fatal("stopped container was inspected")
	}
}
