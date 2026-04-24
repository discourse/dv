package cli

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/docker"
)

const defaultPluginOwner = "discourse"

type pluginSpec struct {
	Input string
	Repo  string
	Path  string
}

func resolvePluginSpec(input string) (pluginSpec, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return pluginSpec{}, fmt.Errorf("plugin cannot be empty")
	}

	repo := input
	shorthand := strings.TrimSuffix(input, ".git")
	switch {
	case looksLikeGitURL(input):
		// Use exact URL/SSH ref.
	case strings.Count(input, "/") == 1:
		repo = "https://github.com/" + shorthand + ".git"
	case !strings.Contains(input, "/"):
		repo = "https://github.com/" + defaultPluginOwner + "/" + shorthand + ".git"
	default:
		return pluginSpec{}, fmt.Errorf("unsupported plugin spec %q; use NAME, OWNER/REPO, or a git URL", input)
	}

	name := pluginRepoName(repo)
	if name == "" || name == "." || name == "/" {
		return pluginSpec{}, fmt.Errorf("could not determine plugin directory for %q", input)
	}

	return pluginSpec{
		Input: input,
		Repo:  repo,
		Path:  path.Join("plugins", name),
	}, nil
}

func looksLikeGitURL(s string) bool {
	return strings.Contains(s, "://") || strings.HasPrefix(s, "git@") || strings.HasPrefix(s, "ssh://")
}

func pluginSpecNeedsSSH(spec string) bool {
	return strings.HasPrefix(strings.TrimSpace(spec), "git@") || strings.HasPrefix(strings.TrimSpace(spec), "ssh://")
}

func pluginRepoName(repo string) string {
	repo = strings.TrimSpace(repo)
	if u, err := url.Parse(repo); err == nil && u.Scheme != "" && u.Path != "" {
		repo = u.Path
	} else if idx := strings.LastIndex(repo, ":"); idx >= 0 && strings.Contains(repo[:idx], "@") {
		repo = repo[idx+1:]
	}
	repo = strings.TrimSuffix(repo, "/")
	name := path.Base(repo)
	name = strings.TrimSuffix(name, ".git")
	return name
}

func setupContainerSSHForwarding(cmd *cobra.Command, name, workdir string, required bool) error {
	if required {
		if _, err := docker.ExecOutput(name, workdir, nil, []string{"bash", "-lc", "test -S /tmp/ssh-agent.sock"}); err != nil {
			return fmt.Errorf("SSH plugin URLs require SSH agent forwarding in the container; use HTTPS or create the agent with SSH forwarding")
		}
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Setting up SSH agent forwarding...\n")
	// Change ownership of the SSH socket to discourse user (it's forwarded from
	// the host with permissions that don't match the container's discourse user).
	if _, err := docker.ExecAsRoot(name, workdir, nil, []string{"chown", "discourse:discourse", "/tmp/ssh-agent.sock"}); err != nil {
		if required {
			return fmt.Errorf("failed to prepare SSH agent socket in container: %w", err)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to chown SSH socket: %v\n", err)
	}
	sshSetup := `
mkdir -p ~/.ssh
chmod 700 ~/.ssh
ssh-keyscan github.com >> ~/.ssh/known_hosts 2>/dev/null
`
	if _, err := docker.ExecOutput(name, workdir, nil, []string{"bash", "-lc", sshSetup}); err != nil {
		if required {
			return fmt.Errorf("failed to setup SSH known_hosts in container: %w", err)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to setup SSH known_hosts: %v\n", err)
	}
	return nil
}

func stopServicesForProvisioning(cmd *cobra.Command, name, workdir string) func() {
	fmt.Fprintf(cmd.OutOrStdout(), "Stopping services for provisioning...\n")
	stopScript := "sudo /usr/bin/sv force-stop pitchfork ember-cli || true"
	if _, err := docker.ExecOutput(name, workdir, nil, []string{"bash", "-lc", stopScript}); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to stop services: %v\n", err)
	}
	return func() {
		fmt.Fprintf(cmd.OutOrStdout(), "Starting services (cleanup)...\n")
		startScript := "sudo /usr/bin/sv start pitchfork ember-cli || true"
		_, _ = docker.ExecOutput(name, workdir, nil, []string{"bash", "-lc", startScript})
	}
}

func installPlugins(cmd *cobra.Command, containerName, workdir string, envs docker.Envs, plugins []templatePlugin) error {
	for _, p := range plugins {
		pPath := strings.TrimSpace(p.Path)
		if pPath == "" {
			pPath = path.Join("plugins", pluginRepoName(p.Repo))
		}
		if pPath == "" || pPath == "plugins" || pPath == "." {
			return fmt.Errorf("could not determine plugin path for %s", p.Repo)
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Installing plugin %s into %s...\n", p.Repo, pPath)
		cloneCmd := buildPluginCloneScript(p.Repo, pPath, p.Branch)
		if err := docker.ExecInteractive(containerName, workdir, envs, []string{"bash", "-lc", cloneCmd}); err != nil {
			return fmt.Errorf("failed to clone plugin %s: %w", p.Repo, err)
		}
	}
	return nil
}

func buildPluginCloneScript(repo, dst, branch string) string {
	cloneArgs := []string{"git", "clone"}
	if branch != "" {
		cloneArgs = append(cloneArgs, "-b", branch)
	}
	cloneArgs = append(cloneArgs, repo, dst)
	return fmt.Sprintf(`
set -e
mkdir -p plugins
if [ -e %s ] && [ "$(ls -A %s 2>/dev/null)" ]; then
  printf '%%s\n' %s >&2
  exit 1
fi
%s
`, shellQuote(dst), shellQuote(dst), shellQuote("Plugin destination already exists and is not empty: "+dst), shellJoin(cloneArgs))
}

func resolvePluginSpecs(inputs []string) ([]templatePlugin, error) {
	plugins := make([]templatePlugin, 0, len(inputs))
	seenPaths := map[string]string{}
	for _, input := range inputs {
		spec, err := resolvePluginSpec(input)
		if err != nil {
			return nil, err
		}
		if prev, ok := seenPaths[spec.Path]; ok {
			return nil, fmt.Errorf("plugins %q and %q both resolve to %s", prev, input, spec.Path)
		}
		seenPaths[spec.Path] = input
		plugins = append(plugins, templatePlugin{Repo: spec.Repo, Path: spec.Path})
	}
	return plugins, nil
}
