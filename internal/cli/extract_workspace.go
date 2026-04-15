package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/docker"
)

type workspaceExtractOptions struct {
	cmd              *cobra.Command
	containerName    string
	containerWorkdir string
	localRepo        string
	branchName       string
	displayName      string
	chdir            bool
	echoCd           bool
	syncMode         bool
	syncDebug        bool
}

func extractWorkspaceRepo(opts workspaceExtractOptions) error {
	var logOut io.Writer = opts.cmd.OutOrStdout()
	var procOut io.Writer = opts.cmd.OutOrStdout()
	var procErr io.Writer = opts.cmd.ErrOrStderr()
	if opts.echoCd {
		logOut = io.Discard
		procOut = opts.cmd.ErrOrStderr()
		procErr = opts.cmd.ErrOrStderr()
	}

	isRepoOut, _ := docker.ExecOutput(opts.containerName, opts.containerWorkdir, nil, []string{"bash", "-lc", "git rev-parse --is-inside-work-tree >/dev/null 2>&1 && echo true || echo false"})
	isRepo := strings.Contains(strings.ToLower(isRepoOut), "true")
	if !isRepo {
		return copyWorkspaceDirectory(opts, logOut, "Workspace is not a git repository", false)
	}
	if opts.syncMode {
		cleanup, err := registerExtractSync(opts.cmd, syncOptions{
			containerName:    opts.containerName,
			containerWorkdir: opts.containerWorkdir,
			localRepo:        opts.localRepo,
			logOut:           logOut,
			errOut:           opts.cmd.ErrOrStderr(),
			debug:            opts.syncDebug,
		})
		if err != nil {
			return err
		}
		defer cleanup()
	}

	status, err := docker.ExecOutput(opts.containerName, opts.containerWorkdir, nil, []string{"git", "status", "--porcelain", "-z", "--untracked-files=all"})
	if err != nil {
		return err
	}
	if status == "" {
		if opts.syncMode {
			status = ""
		} else {
			return fmt.Errorf("no changes detected in %s", opts.containerWorkdir)
		}
	}

	repoCloneURL, _ := docker.ExecOutput(opts.containerName, opts.containerWorkdir, nil, []string{"bash", "-lc", "git config --get remote.origin.url"})
	repoCloneURL = strings.TrimSpace(repoCloneURL)
	if repoCloneURL == "" {
		return copyWorkspaceDirectory(opts, logOut, "No git remote detected; copying entire workspace", true)
	}

	if _, err := os.Stat(opts.localRepo); os.IsNotExist(err) {
		candidates := makeCloneCandidates(repoCloneURL)
		fmt.Fprintf(logOut, "Cloning (trying %d URL(s))...\n", len(candidates))
		if err := cloneWithFallback(procOut, procErr, candidates, opts.localRepo); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(logOut, "Using existing repo, resetting...")
		if err := runInDir(opts.localRepo, procOut, procErr, "git", "reset", "--hard", "HEAD"); err != nil {
			return err
		}
		if err := runInDir(opts.localRepo, procOut, procErr, "git", "clean", "-fd"); err != nil {
			return err
		}
		if err := runInDir(opts.localRepo, procOut, procErr, "git", "fetch", "origin"); err != nil {
			return err
		}
	}

	commit, err := docker.ExecOutput(opts.containerName, opts.containerWorkdir, nil, []string{"bash", "-lc", "git rev-parse HEAD"})
	if err != nil {
		return err
	}
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return fmt.Errorf("unable to determine commit in %s", opts.containerWorkdir)
	}

	containerBranch, err := docker.ExecOutput(opts.containerName, opts.containerWorkdir, nil, []string{"bash", "-lc", "git rev-parse --abbrev-ref HEAD"})
	if err != nil {
		containerBranch = ""
	}
	containerBranch = strings.TrimSpace(containerBranch)

	fmt.Fprintf(logOut, "%s commit: %s\n", opts.displayName, commit)
	if containerBranch != "" {
		fmt.Fprintf(logOut, "%s branch: %s\n", opts.displayName, containerBranch)
	}

	branchDisplay := ""
	if commitExistsInRepo(opts.localRepo, commit) {
		if containerBranch != "" && containerBranch != "HEAD" {
			if err := runInDir(opts.localRepo, procOut, procErr, "git", "checkout", "-B", containerBranch, commit); err != nil {
				return err
			}
			branchDisplay = containerBranch
		} else {
			if err := runInDir(opts.localRepo, procOut, procErr, "git", "checkout", "--detach", commit); err != nil {
				return err
			}
			branchDisplay = "HEAD (detached)"
		}
	} else {
		// Commit missing - try to fetch from container first (handles rebased commits)
		ctx := opts.cmd.Context()
		syncErr := syncFromContainer(ctx, opts.containerName, opts.containerWorkdir, opts.localRepo, commit, logOut, opts.syncDebug)
		if syncErr == nil && commitExistsInRepo(opts.localRepo, commit) {
			// Sync succeeded and commit now exists - do normal checkout
			if containerBranch != "" && containerBranch != "HEAD" {
				if err := runInDir(opts.localRepo, procOut, procErr, "git", "checkout", "-B", containerBranch, commit); err != nil {
					return err
				}
				branchDisplay = containerBranch
			} else {
				if err := runInDir(opts.localRepo, procOut, procErr, "git", "checkout", "--detach", commit); err != nil {
					return err
				}
				branchDisplay = "HEAD (detached)"
			}
		} else {
			// Fall back to creating branch from origin - commit doesn't exist in local repo
			if opts.syncDebug && syncErr != nil {
				fmt.Fprintf(logOut, "[git-sync] sync from container failed: %v\n", syncErr)
			}
			baseRef := ""
			if containerBranch != "" && containerBranch != "HEAD" {
				candidate := "origin/" + containerBranch
				if refExists(opts.localRepo, candidate) {
					baseRef = candidate
				}
			}
			if baseRef == "" {
				switch {
				case refExists(opts.localRepo, "origin/main"):
					baseRef = "origin/main"
				case refExists(opts.localRepo, "origin/master"):
					baseRef = "origin/master"
				case refExists(opts.localRepo, "origin/HEAD"):
					baseRef = "origin/HEAD"
				}
			}
			if baseRef != "" {
				if err := runInDir(opts.localRepo, procOut, procErr, "git", "checkout", "-B", opts.branchName, baseRef); err != nil {
					return err
				}
			} else {
				if err := runInDir(opts.localRepo, procOut, procErr, "git", "checkout", "-B", opts.branchName); err != nil {
					return err
				}
			}
			branchDisplay = opts.branchName
		}
	}

	fmt.Fprintf(logOut, "Extracting %s changes from container...\n", opts.displayName)
	changedCount, err := applyExtractStatus(logOut, opts.containerName, opts.containerWorkdir, opts.localRepo, status)
	if err != nil {
		return err
	}

	if opts.echoCd {
		fmt.Fprintf(opts.cmd.OutOrStdout(), "cd %s\n", opts.localRepo)
		return nil
	}

	fmt.Fprintln(logOut, "")
	fmt.Fprintf(logOut, "✅ %s changes extracted successfully!\n", opts.displayName)
	fmt.Fprintf(logOut, "📁 Location: %s\n", opts.localRepo)
	if strings.TrimSpace(branchDisplay) != "" {
		fmt.Fprintf(logOut, "🌿 Branch: %s\n", branchDisplay)
	}
	fmt.Fprintf(logOut, "📊 Files changed: %d\n", changedCount)
	fmt.Fprintf(logOut, "🎯 Base commit: %s\n", commit)

	if opts.syncMode {
		if changedCount == 0 {
			fmt.Fprintln(logOut, "No pending changes detected; watching for new modifications...")
		}
		fmt.Fprintln(logOut, "🔄 Entering sync mode; press Ctrl+C to stop.")
		return runExtractSync(opts.cmd, syncOptions{
			containerName:    opts.containerName,
			containerWorkdir: opts.containerWorkdir,
			localRepo:        opts.localRepo,
			logOut:           logOut,
			errOut:           opts.cmd.ErrOrStderr(),
			debug:            opts.syncDebug,
		})
	}

	if opts.chdir {
		shell := os.Getenv("SHELL")
		if strings.TrimSpace(shell) == "" {
			shell = "/bin/bash"
		}
		s := exec.Command(shell)
		s.Dir = opts.localRepo
		s.Stdin = os.Stdin
		s.Stdout = os.Stdout
		s.Stderr = os.Stderr
		return s.Run()
	}

	return nil
}

func copyWorkspaceDirectory(opts workspaceExtractOptions, logOut io.Writer, reason string, allowSync bool) error {
	if reason != "" {
		fmt.Fprintln(logOut, reason)
	}
	_ = os.RemoveAll(opts.localRepo)
	if err := os.MkdirAll(opts.localRepo, 0o755); err != nil {
		return err
	}
	if err := docker.CopyFromContainer(opts.containerName, containerCopyAllSource(opts.containerWorkdir), opts.localRepo); err != nil {
		return err
	}

	if opts.echoCd {
		fmt.Fprintf(opts.cmd.OutOrStdout(), "cd %s\n", opts.localRepo)
		return nil
	}

	fmt.Fprintln(logOut, "")
	fmt.Fprintf(logOut, "✅ %s copied successfully!\n", opts.displayName)
	fmt.Fprintf(logOut, "📁 Location: %s\n", opts.localRepo)

	if opts.syncMode {
		if !allowSync {
			fmt.Fprintln(logOut, "Sync mode requires a git repository; skipping.")
		} else {
			fmt.Fprintln(logOut, "🔄 Entering sync mode; press Ctrl+C to stop.")
			return runExtractSync(opts.cmd, syncOptions{
				containerName:    opts.containerName,
				containerWorkdir: opts.containerWorkdir,
				localRepo:        opts.localRepo,
				logOut:           logOut,
				errOut:           opts.cmd.ErrOrStderr(),
				debug:            opts.syncDebug,
			})
		}
	}

	if opts.chdir {
		shell := os.Getenv("SHELL")
		if strings.TrimSpace(shell) == "" {
			shell = "/bin/bash"
		}
		s := exec.Command(shell)
		s.Dir = opts.localRepo
		s.Stdin = os.Stdin
		s.Stdout = os.Stdout
		s.Stderr = os.Stderr
		return s.Run()
	}

	return nil
}

func workspaceLocalPath(dataDir, workdir string) string {
	base := filepath.Base(workdir)
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "workspace"
	}
	slug := themeDirSlug(base)
	return filepath.Join(dataDir, fmt.Sprintf("%s_src", slug))
}

func containerCopyAllSource(dir string) string {
	trimmed := strings.TrimRight(dir, "/")
	if trimmed == "" {
		return "/."
	}
	return trimmed + "/."
}
