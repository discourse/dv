package cli

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

var copyCmd = &cobra.Command{
	Use:     "copy SOURCE DEST",
	Aliases: []string{"cp"},
	Short:   "Copy files between host and container",
	Long: `Copy files or directories between host and container.

Syntax:
  dv cp <host-path> <container-path>     Copy host → container
  dv cp @:<container-path> <host-path>   Copy container → host (selected container)
  dv cp <name>:<path> <host-path>        Copy container → host (named container)
  dv cp <host-path> <name>:<path>        Copy host → named container

Examples:
  dv cp ./file.rb /var/www/discourse/    Copy from host to selected container
  dv cp @:/var/www/discourse/log ./      Copy from selected container to host
  dv cp agent-2:/tmp/file.txt ./         Copy from agent-2 container to host`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		src := args[0]
		dst := args[1]
		verbose, _ := cmd.Flags().GetBool("verbose")

		// Resolve config
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}

		// Parse source and destination
		srcContainer, srcPath := parseContainerPath(src)
		dstContainer, dstPath := parseContainerPath(dst)

		// Resolve @ to selected container
		if srcContainer == "@" {
			srcContainer = currentAgentName(cfg)
		}
		if dstContainer == "@" {
			dstContainer = currentAgentName(cfg)
		}

		// Handle --name flag override for backward compatibility
		nameFlag, _ := cmd.Flags().GetString("name")
		if nameFlag != "" {
			if srcContainer == "" && dstContainer == "" {
				// Old behavior: host → container
				dstContainer = nameFlag
			}
		}

		// Determine direction
		if srcContainer != "" && dstContainer != "" {
			return fmt.Errorf("cannot specify container on both source and destination")
		}

		if srcContainer == "" && dstContainer == "" {
			// Default: host → selected container
			dstContainer = currentAgentName(cfg)
			return copyHostToContainer(src, dstPath, dstContainer, verbose)
		}

		if srcContainer != "" {
			// Container → host
			if !docker.Running(srcContainer) {
				return fmt.Errorf("container '%s' is not running; run 'dv start' first", srcContainer)
			}
			return copyContainerToHost(srcContainer, srcPath, dstPath, verbose)
		}

		// Host → container
		if !docker.Running(dstContainer) {
			return fmt.Errorf("container '%s' is not running; run 'dv start' first", dstContainer)
		}
		return copyHostToContainer(src, dstPath, dstContainer, verbose)
	},
}

// parseContainerPath splits "container:path" into (container, path).
// Returns ("", path) if no colon is present.
// "@:path" returns ("@", path).
func parseContainerPath(arg string) (container, path string) {
	idx := strings.Index(arg, ":")
	if idx == -1 {
		return "", arg
	}
	return arg[:idx], arg[idx+1:]
}

func copyHostToContainer(srcOnHost, dstInContainer, containerName string, verbose bool) error {
	// Validate source exists on host
	if _, err := os.Stat(srcOnHost); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("source path does not exist: %s", srcOnHost)
		}
		return fmt.Errorf("failed to stat source path: %w", err)
	}

	// docker cp requires an existing parent directory, which the image does not ship.
	if parent := containerParentDir(dstInContainer); parent != "" {
		if _, err := docker.ExecOutput(containerName, "/", nil, []string{"bash", "-lc", "mkdir -p " + shellQuote(parent)}); err != nil {
			return fmt.Errorf("failed to create directory %s in container %s: %w", parent, containerName, err)
		}
	}

	// Copy with recursive ownership set to discourse:discourse
	if err := docker.CopyToContainerWithOwnership(containerName, srcOnHost, dstInContainer, true); err != nil {
		return fmt.Errorf("failed to copy %s to container %s:%s: %w", srcOnHost, containerName, dstInContainer, err)
	}

	if verbose {
		fmt.Printf("Copied %s → %s:%s\n", srcOnHost, containerName, dstInContainer)
	}
	return nil
}

// containerParentDir returns the in-container directory that must exist before
// dst can be copied to, or "" when there is nothing to create. A trailing slash
// yields the directory itself; root-level and bare relative names have none.
func containerParentDir(dst string) string {
	if dst == "" {
		return ""
	}
	parent := path.Dir(dst)
	if parent == "/" || parent == "." {
		return ""
	}
	return parent
}

func copyContainerToHost(containerName, srcInContainer, dstOnHost string, verbose bool) error {
	// Check if source path needs shell expansion (contains glob metacharacters or ~)
	needsExpansion := docker.ContainsGlobMeta(srcInContainer) || strings.HasPrefix(srcInContainer, "~")

	if !needsExpansion {
		// Simple case: no glob, copy directly
		if err := docker.CopyFromContainer(containerName, srcInContainer, dstOnHost); err != nil {
			return fmt.Errorf("failed to copy %s:%s to %s: %w", containerName, srcInContainer, dstOnHost, err)
		}
		if verbose {
			fmt.Printf("Copied %s:%s → %s\n", containerName, srcInContainer, dstOnHost)
		}
		return nil
	}

	// Expand glob pattern in container
	paths, err := docker.ExpandGlobInContainer(containerName, srcInContainer)
	if err != nil {
		return fmt.Errorf("failed to expand pattern '%s' in container: %w", srcInContainer, err)
	}
	if len(paths) == 0 {
		return fmt.Errorf("no files match pattern: %s", srcInContainer)
	}

	// When copying multiple files, destination must be an existing directory
	if len(paths) > 1 {
		st, err := os.Stat(dstOnHost)
		if err != nil || !st.IsDir() {
			return fmt.Errorf("destination must be an existing directory when copying multiple files: %s", dstOnHost)
		}
	}

	// Copy each matched file
	for _, path := range paths {
		if err := docker.CopyFromContainer(containerName, path, dstOnHost); err != nil {
			return fmt.Errorf("failed to copy %s:%s to %s: %w", containerName, path, dstOnHost, err)
		}
		if verbose {
			fmt.Printf("Copied %s:%s → %s\n", containerName, path, filepath.Join(dstOnHost, filepath.Base(path)))
		}
	}

	if !verbose && len(paths) > 0 {
		fmt.Printf("Copied %d file(s) from %s\n", len(paths), srcInContainer)
	}
	return nil
}

func init() {
	copyCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	copyCmd.Flags().BoolP("verbose", "v", false, "Print progress messages")
}
