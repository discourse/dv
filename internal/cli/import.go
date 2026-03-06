package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

// importCmd copies the full chain of commits since the base branch and the
// current working tree changes from the host git repo (current directory) into
// the running container's repo, reproducing the branch and commit history.
var importCmd = &cobra.Command{
	Use:   "import",
	Short: "Import local git commits and working changes into the container",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		verbose, _ := cmd.Flags().GetBool("verbose")
		verbose = verbose || isTruthyEnv("DV_VERBOSE")
		stderr := cmd.ErrOrStderr()

		// Resolve config and container
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}

		name, _ := cmd.Flags().GetString("name")
		if name == "" {
			name = currentAgentName(cfg)
		}

		// Ensure the container is running for the selected image
		if err := ensureContainerRunning(cmd, cfg, name, false, ""); err != nil {
			return err
		}

		// Determine workdir and image kind
		imgName := cfg.ContainerImages[name]
		_, imgCfg, err := resolveImage(cfg, imgName)
		if err != nil {
			return err
		}
		workdir := imgCfg.Workdir

		if verbose {
			fmt.Fprintf(stderr, "[verbose] container=%s workdir=%s image=%s\n", name, workdir, imgName)
		}

		// Host repo context (current directory)
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}

		// Validate git repository
		if ok, _ := runHostCapture(cwd, "git", "rev-parse", "--is-inside-work-tree"); strings.TrimSpace(ok) != "true" {
			return fmt.Errorf("current directory is not inside a git repository: %s", cwd)
		}

		// Discover repo top-level
		repoRoot, err := runHostCapture(cwd, "git", "rev-parse", "--show-toplevel")
		if err != nil {
			return fmt.Errorf("failed to resolve repository root: %v", err)
		}
		repoRoot = strings.TrimSpace(repoRoot)

		// Capture host git identity to reuse inside the container if needed
		hostUserName, _ := runHostCapture(repoRoot, "git", "config", "--get", "user.name")
		hostUserEmail, _ := runHostCapture(repoRoot, "git", "config", "--get", "user.email")
		hostUserName = strings.TrimSpace(hostUserName)
		hostUserEmail = strings.TrimSpace(hostUserEmail)

		// Determine current branch
		branch, err := runHostCapture(cwd, "git", "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return fmt.Errorf("failed to determine current branch: %v", err)
		}
		branch = strings.TrimSpace(branch)
		if branch == "HEAD" || branch == "" {
			branch = fmt.Sprintf("dv-import-%s", time.Now().Format("20060102-150405"))
		}

		// Determine base branch/ref
		base, _ := cmd.Flags().GetString("base")
		if strings.TrimSpace(base) == "" {
			base = "main"
		}

		// Resolve base SHA from local refs (prefer local branch, then origin/base)
		baseRef := base
		if _, err := runHostCapture(cwd, "git", "rev-parse", "--verify", baseRef); err != nil {
			// try origin/base
			if _, err2 := runHostCapture(cwd, "git", "rev-parse", "--verify", "origin/"+base); err2 == nil {
				baseRef = "origin/" + base
			} else {
				return fmt.Errorf("could not resolve base ref '%s' or 'origin/%s'", base, base)
			}
		}
		baseSha, err := runHostCapture(cwd, "git", "rev-parse", baseRef)
		if err != nil {
			return fmt.Errorf("failed to resolve base sha for %s: %v", baseRef, err)
		}
		baseSha = strings.TrimSpace(baseSha)

		if verbose {
			fmt.Fprintf(stderr, "[verbose] branch=%s baseRef=%s baseSha=%s\n", branch, baseRef, baseSha)
		}

		// We will generate patches relative to the resolved base SHA so we can
		// apply them on top of the exact same commit inside the container.

		// Prepare temporary directory for patches
		tmpDir, err := os.MkdirTemp("", "dv-import-")
		if err != nil {
			return err
		}
		// Do not defer removal; leave artifacts in case of failure for debugging

		patchesDir := filepath.Join(tmpDir, "patches")
		if err := os.MkdirAll(patchesDir, 0o755); err != nil {
			return err
		}

		// Create patch series for commits since baseSha
		// This preserves authorship and messages
		if err := runInDir(repoRoot, cmd.OutOrStdout(), cmd.ErrOrStderr(), "git", "format-patch", "--binary", "-o", patchesDir, baseSha+"..HEAD"); err != nil {
			// If there are no commits, format-patch exits 0 without creating files; other errors are real
			// We'll continue even if no files were created
		}

		// Collect patch files to know if we have any
		patchFiles, _ := os.ReadDir(patchesDir)

		if verbose {
			fmt.Fprintf(stderr, "[verbose] patches=%d tmpDir=%s\n", len(patchFiles), tmpDir)
		}

		// Prepare working tree change list from porcelain status
		statusOut, err := runHostCapture(repoRoot, "git", "status", "--porcelain")
		if err != nil {
			return fmt.Errorf("failed to obtain status: %v", err)
		}

		var copies []string
		var deletes []string
		scanner := bufio.NewScanner(strings.NewReader(statusOut))
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}
			if len(line) < 3 {
				continue
			}
			codes := line[:2]
			rest := strings.TrimSpace(line[3:])
			// Handle rename syntax: "old -> new"
			if strings.Contains(rest, " -> ") {
				parts := strings.SplitN(rest, " -> ", 2)
				oldPath := strings.TrimSpace(parts[0])
				newPath := strings.TrimSpace(parts[1])
				if oldPath != "" {
					deletes = append(deletes, oldPath)
				}
				if newPath != "" {
					copies = append(copies, newPath)
				}
				continue
			}
			// Determine operations from status codes
			if codes == "??" || strings.ContainsAny(codes, "AM") || strings.Contains(codes, "R") || strings.Contains(codes, "C") || strings.Contains(codes, "U") || strings.Contains(codes, "M") {
				// Treat anything indicating an added/modified/renamed/copied/updated path as needing copy
				copies = append(copies, rest)
			}
			if strings.Contains(codes, "D") {
				deletes = append(deletes, rest)
			}
		}
		if err := scanner.Err(); err != nil {
			return err
		}

		// Deduplicate and normalize lists
		copies = uniqueStrings(expandExistingPaths(repoRoot, copies))
		deletes = uniqueStrings(deletes)

		if verbose {
			fmt.Fprintf(stderr, "[verbose] status: copies=%d deletes=%d\n", len(copies), len(deletes))
		}

		// Copy patches directory to container under /tmp
		// Resulting container path will be /tmp/<basename(tmpDir)>/patches
		tmpBase := filepath.Base(tmpDir)
		if err := docker.CopyToContainerWithOwnership(name, tmpDir, "/tmp", true); err != nil {
			return fmt.Errorf("failed to copy patches to container: %v", err)
		}
		inContainerTmp := filepath.Join("/tmp", tmpBase)
		inContainerPatches := filepath.Join(inContainerTmp, "patches")
		if _, err := docker.ExecAsRoot(name, "/", nil, []string{"chmod", "-R", "755", inContainerTmp}); err != nil {
			return fmt.Errorf("failed to set permissions on patches directory: %v", err)
		}

		// Inside container: ensure a clean state and the base SHA is available,
		// then align base branch and create/checkout current branch at base
		if verbose {
			fmt.Fprintf(stderr, "[verbose] resetting container working tree\n")
		}
		if out, err := docker.ExecCombinedOutput(name, workdir, nil, []string{"bash", "-lc", "git reset --hard && git clean -fd"}); err != nil {
			return fmt.Errorf("container: failed to reset working tree: %v\n%s", err, strings.TrimSpace(out))
		}
		// Ensure we have the full history needed for 3-way application
		// 1) Make sure origin fetches all branches
		_, _ = docker.ExecOutput(name, workdir, nil, []string{"bash", "-lc", "git config remote.origin.fetch '+refs/heads/*:refs/remotes/origin/*'"})
		// 2) De-shallow if necessary, otherwise perform a normal fetch
		if verbose {
			fmt.Fprintf(stderr, "[verbose] fetching origin (with shallow detection)\n")
		}
		if out, err := docker.ExecCombinedOutput(name, workdir, nil, []string{"bash", "-lc", "if [ -f .git/shallow ]; then git fetch origin --tags --prune --force --unshallow; else git fetch origin --tags --prune --force; fi"}); err != nil {
			return fmt.Errorf("container: failed to fetch refs: %v\n%s", err, strings.TrimSpace(out))
		}
		// 3) Force-align base branch name to the exact baseSha without failing when already checked out
		alignCmd := strings.Join([]string{
			"set -euo pipefail",
			fmt.Sprintf("branch=%s", shellQuote(base)),
			fmt.Sprintf("sha=%s", shellQuote(baseSha)),
			"current=$(git symbolic-ref --quiet --short HEAD || true)",
			"if [ \"$current\" != \"$branch\" ]; then",
			"\tif git show-ref --verify --quiet \"refs/heads/$branch\"; then",
			"\t\tgit branch -f \"$branch\" \"$sha\"",
			"\telse",
			"\t\tgit branch \"$branch\" \"$sha\"",
			"\tfi",
			"\tgit checkout \"$branch\"",
			"fi",
			"git reset --hard \"$sha\"",
		}, "\n")
		if verbose {
			fmt.Fprintf(stderr, "[verbose] aligning base branch %s to %s\n", base, baseSha[:min(12, len(baseSha))])
		}
		if out, err := docker.ExecCombinedOutput(name, workdir, nil, []string{"bash", "-lc", alignCmd}); err != nil {
			return fmt.Errorf("container: failed to set base branch %s to %s: %v\n%s", base, baseSha, err, strings.TrimSpace(out))
		}
		if verbose {
			fmt.Fprintf(stderr, "[verbose] checking out and resetting base %s\n", base)
		}
		if out, err := docker.ExecCombinedOutput(name, workdir, nil, []string{"bash", "-lc", fmt.Sprintf("git checkout %s && git reset --hard %s", shellQuote(base), shellQuote(baseSha))}); err != nil {
			return fmt.Errorf("container: failed to checkout/reset base %s at %s: %v\n%s", base, baseSha, err, strings.TrimSpace(out))
		}
		// 4) Create/reset the working branch to start at baseSha
		if verbose {
			fmt.Fprintf(stderr, "[verbose] creating branch %s at %s\n", branch, baseSha[:min(12, len(baseSha))])
		}
		if out, err := docker.ExecCombinedOutput(name, workdir, nil, []string{"bash", "-lc", fmt.Sprintf("git checkout -B %s %s", shellQuote(branch), shellQuote(baseSha))}); err != nil {
			return fmt.Errorf("container: failed to checkout branch %s: %v\n%s", branch, err, strings.TrimSpace(out))
		}

		// Preflight: ensure git identity is set inside the container repo so git am can commit
		// Prefer host identity; otherwise use sensible fallbacks.
		{
			if verbose {
				fmt.Fprintf(stderr, "[verbose] checking container git identity\n")
			}
			getCfg := func(key string) string {
				out, _ := docker.ExecOutput(name, workdir, nil, []string{"bash", "-lc", fmt.Sprintf("git config --get %s || true", shellQuote(key))})
				return strings.TrimSpace(out)
			}
			setCfg := func(key, val string) {
				if strings.TrimSpace(val) == "" {
					return
				}
				_, _ = docker.ExecOutput(name, workdir, nil, []string{"bash", "-lc", fmt.Sprintf("git config %s %s", shellQuote(key), shellQuote(val))})
			}

			cName := getCfg("user.name")
			cEmail := getCfg("user.email")
			if cName == "" {
				val := hostUserName
				if val == "" {
					val = "dv importer"
				}
				setCfg("user.name", val)
				if verbose {
					fmt.Fprintf(stderr, "[verbose] set container user.name=%s\n", val)
				}
			} else if verbose {
				fmt.Fprintf(stderr, "[verbose] container user.name already set: %s\n", cName)
			}
			if cEmail == "" {
				val := hostUserEmail
				if val == "" {
					val = "dv-importer@example.invalid"
				}
				setCfg("user.email", val)
				if verbose {
					fmt.Fprintf(stderr, "[verbose] set container user.email=%s\n", val)
				}
			} else if verbose {
				fmt.Fprintf(stderr, "[verbose] container user.email already set: %s\n", cEmail)
			}
		}

		// Apply patches if any
		if len(patchFiles) > 0 {
			if verbose {
				fmt.Fprintf(stderr, "[verbose] applying %d patch(es) via git am\n", len(patchFiles))
			}
			// Use glob expansion in shell for *.patch
			cmdline := fmt.Sprintf("set -euo pipefail; shopt -s nullglob; files=(%s/*.patch); if (( ${#files[@]} > 0 )); then git am --3way --committer-date-is-author-date --whitespace=nowarn --no-gpg-sign \"${files[@]}\"; fi", inContainerPatches)
			if out, err := docker.ExecCombinedOutput(name, workdir, nil, []string{"bash", "-lc", cmdline}); err != nil {
				// Attempt to abort to leave repo clean
				_, _ = docker.ExecOutput(name, workdir, nil, []string{"bash", "-lc", "git am --abort || true"})
				// Capture current git identity for diagnostics
				cNameOut, _ := docker.ExecOutput(name, workdir, nil, []string{"bash", "-lc", "git config --get user.name || true"})
				cEmailOut, _ := docker.ExecOutput(name, workdir, nil, []string{"bash", "-lc", "git config --get user.email || true"})
				cName := strings.TrimSpace(cNameOut)
				cEmail := strings.TrimSpace(cEmailOut)
				return fmt.Errorf(
					"container: failed to apply patches (git am): %v\n%s\n\ncontainer git identity: user.name=%q, user.email=%q\nIf the error mentions 'Author identity unknown', set identity with:\n  dv run -- git config user.email 'you@example.com' && git config user.name 'Your Name'",
					err,
					strings.TrimSpace(out),
					cName,
					cEmail,
				)
			}
		}

		if verbose && len(patchFiles) > 0 {
			fmt.Fprintf(stderr, "[verbose] patches applied successfully\n")
		}

		// Apply working tree changes: create directories, copy files, delete paths
		if len(copies) > 0 {
			// Prepare directories to create inside container
			dirSet := map[string]struct{}{}
			for _, rel := range copies {
				d := filepath.Dir(rel)
				if d == "." || d == "" {
					continue
				}
				dirSet[d] = struct{}{}
			}
			if len(dirSet) > 0 {
				var dirs []string
				for d := range dirSet {
					dirs = append(dirs, d)
				}
				sort.Strings(dirs)
				// Build mkdir -p command
				var quoted []string
				for _, d := range dirs {
					quoted = append(quoted, shellQuote(filepath.Join(workdir, d)))
				}
				mkdirCmd := fmt.Sprintf("mkdir -p %s", strings.Join(quoted, " "))
				if verbose {
					fmt.Fprintf(stderr, "[verbose] creating %d directories in container\n", len(dirs))
				}
				if out, err := docker.ExecCombinedOutput(name, workdir, nil, []string{"bash", "-lc", mkdirCmd}); err != nil {
					return fmt.Errorf("container: failed to create directories: %v\n%s", err, strings.TrimSpace(out))
				}
			}
			// Copy files one-by-one to preserve content
			if verbose {
				fmt.Fprintf(stderr, "[verbose] copying %d file(s) to container\n", len(copies))
			}
			for _, rel := range copies {
				src := filepath.Join(repoRoot, rel)
				dst := filepath.Join(workdir, rel)
				if err := docker.CopyToContainer(name, src, dst); err != nil {
					return fmt.Errorf("failed to copy %s: %v", rel, err)
				}
			}
		}
		if len(deletes) > 0 {
			if verbose {
				fmt.Fprintf(stderr, "[verbose] deleting %d file(s) in container\n", len(deletes))
			}
			// Remove files inside container
			var quoted []string
			for _, rel := range deletes {
				quoted = append(quoted, shellQuote(filepath.Join(workdir, rel)))
			}
			rmCmd := fmt.Sprintf("rm -f %s", strings.Join(quoted, " "))
			if out, err := docker.ExecCombinedOutput(name, workdir, nil, []string{"bash", "-lc", rmCmd}); err != nil {
				return fmt.Errorf("container: failed to delete files: %v\n%s", err, strings.TrimSpace(out))
			}
		}

		// Final output summary
		fmt.Fprintf(cmd.OutOrStdout(), "📦 Imported into container '%s' at %s\n", name, workdir)
		fmt.Fprintf(cmd.OutOrStdout(), "🌿 Base: %s (%s)\n", base, baseSha[:min(12, len(baseSha))])
		fmt.Fprintf(cmd.OutOrStdout(), "🔀 Branch: %s\n", branch)
		fmt.Fprintf(cmd.OutOrStdout(), "🧱 Commits applied: %d\n", len(patchFiles))
		fmt.Fprintf(cmd.OutOrStdout(), "📄 Working changes copied: %d, deleted: %d\n", len(copies), len(deletes))

		return nil
	},
}

func init() {
	importCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	importCmd.Flags().String("base", "main", "Base branch to diff against (default: main)")
	importCmd.Flags().Bool("verbose", false, "Show detailed debugging information")
}

// --- helpers ---

func runHostCapture(dir string, name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		// Include stderr/stdout in error for easier debugging
		return "", errors.New(strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func expandExistingPaths(root string, rels []string) []string {
	var out []string
	for _, rel := range rels {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		abs := filepath.Join(root, rel)
		info, err := os.Lstat(abs)
		if err != nil {
			continue
		}
		// Follow symlinks to capture the pointed-to type
		if info.Mode()&fs.ModeSymlink != 0 {
			if resolved, err := os.Stat(abs); err == nil {
				info = resolved
			}
		}
		if info.Mode().IsRegular() {
			out = append(out, filepath.ToSlash(rel))
			continue
		}
		if !info.IsDir() {
			continue
		}
		// Walk directories to include contained files
		filepath.WalkDir(abs, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			typeInfo := d.Type()
			if !typeInfo.IsRegular() && typeInfo&fs.ModeSymlink == 0 {
				return nil
			}
			relPath, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			out = append(out, filepath.ToSlash(relPath))
			return nil
		})
	}
	return out
}

func uniqueStrings(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
