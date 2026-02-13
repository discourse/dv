package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/term"
)

// getIdentityAgent parses ~/.ssh/config for a global IdentityAgent setting.
// Returns the expanded path if found, empty string otherwise.
func getIdentityAgent() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	configPath := filepath.Join(home, ".ssh", "config")
	f, err := os.Open(configPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	// Look for IdentityAgent before any Host block (global setting)
	// or in a Host * block
	scanner := bufio.NewScanner(f)
	inGlobalOrWildcard := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "host ") {
			// Check if it's "Host *"
			hostValue := strings.TrimSpace(line[5:])
			inGlobalOrWildcard = hostValue == "*"
			continue
		}
		if inGlobalOrWildcard && strings.HasPrefix(lower, "identityagent ") {
			agent := strings.TrimSpace(line[14:])
			// Remove quotes if present
			agent = strings.Trim(agent, "\"'")
			// Expand ~ to home directory
			if strings.HasPrefix(agent, "~/") {
				agent = filepath.Join(home, agent[2:])
			}
			return agent
		}
	}
	return ""
}

// BuildOptions controls how docker images are built.
type BuildOptions struct {
	ExtraArgs    []string // additional docker build args supplied by callers
	ForceClassic bool     // skip buildx/BuildKit helpers and use legacy docker build
	Builder      string   // optional buildx builder name
}

func Exists(name string) bool {
	out, _ := exec.Command("bash", "-lc", "docker ps -aq -f name=^"+shellEscape(name)+"$").Output()
	return strings.TrimSpace(string(out)) != ""
}

func Running(name string) bool {
	out, _ := exec.Command("bash", "-lc", "docker ps -q -f status=running -f name=^"+shellEscape(name)+"$").Output()
	return strings.TrimSpace(string(out)) != ""
}

func Stop(name string) error {
	if isTruthyEnv("DV_VERBOSE") {
		fmt.Fprintf(os.Stderr, "Running: docker stop %s\n", name)
	}
	cmd := exec.Command("docker", "stop", name)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func Remove(name string) error {
	if isTruthyEnv("DV_VERBOSE") {
		fmt.Fprintf(os.Stderr, "Running: docker rm %s\n", name)
	}
	cmd := exec.Command("docker", "rm", name)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func RemoveForce(name string) error {
	if isTruthyEnv("DV_VERBOSE") {
		fmt.Fprintf(os.Stderr, "Running: docker rm -f %s\n", name)
	}
	cmd := exec.Command("docker", "rm", "-f", name)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func Rename(oldName, newName string) error {
	if isTruthyEnv("DV_VERBOSE") {
		fmt.Fprintf(os.Stderr, "Running: docker rename %s %s\n", oldName, newName)
	}
	cmd := exec.Command("docker", "rename", oldName, newName)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// Pull applies to an image ref (repo:tag or repo@digest)
func Pull(ref string) error {
	if isTruthyEnv("DV_VERBOSE") {
		fmt.Fprintf(os.Stderr, "Running: docker pull %s\n", ref)
	}
	cmd := exec.Command("docker", "pull", ref)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// PullBaseImages parses the Dockerfile at path and attempts to pull all unique
// images found in FROM instructions. It ignores images that refer to
// build stages (AS ...). It prints warnings to stderr on failure but returns nil.
func PullBaseImages(dockerfilePath string, out io.Writer) {
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		return
	}

	stages := make(map[string]bool)
	var toPull []string

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		upper := strings.ToUpper(trimmed)
		if !strings.HasPrefix(upper, "FROM") {
			continue
		}
		fields := strings.Fields(trimmed)
		// FROM [--platform=...] image [AS name]
		idx := 1
		if idx < len(fields) && strings.HasPrefix(fields[idx], "--platform=") {
			idx++
		}
		if idx < len(fields) {
			image := fields[idx]
			if image != "scratch" && !stages[image] && !strings.Contains(image, "$") {
				toPull = append(toPull, image)
			}
			// Check for AS name
			for i := idx + 1; i < len(fields); i++ {
				if strings.ToUpper(fields[i]) == "AS" && i+1 < len(fields) {
					stages[fields[i+1]] = true
					break
				}
			}
		}
	}

	// Pull unique images
	pulled := make(map[string]bool)
	for _, img := range toPull {
		if pulled[img] {
			continue
		}
		fmt.Fprintf(out, "Pulling latest base image %s...\n", img)
		if err := Pull(img); err != nil {
			fmt.Fprintf(out, "Warning: failed to pull %s (%v); continuing with local version if available.\n", img, err)
		}
		pulled[img] = true
	}
}

func Build(tag string, args []string) error {
	argv := []string{"build", "-t", tag}
	argv = append(argv, args...)
	argv = append(argv, ".")
	if isTruthyEnv("DV_VERBOSE") {
		fmt.Fprintf(os.Stderr, "Running: docker %s\n", strings.Join(argv, " "))
	}
	cmd := exec.Command("docker", argv...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// BuildFrom builds a Docker image from a specific Dockerfile and context
// directory. dockerfilePath may be absolute or relative; contextDir must be
// a directory.
func BuildFrom(tag, dockerfilePath, contextDir string, opts BuildOptions) error {
	if !filepath.IsAbs(dockerfilePath) {
		// ensure relative dockerfile path is evaluated relative to contextDir
		dockerfilePath = filepath.Join(contextDir, dockerfilePath)
	}
	if opts.ExtraArgs == nil {
		opts.ExtraArgs = []string{}
	}
	if opts.Builder == "" {
		if env := strings.TrimSpace(os.Getenv("DV_BUILDX_BUILDER")); env != "" {
			opts.Builder = env
		} else if env := strings.TrimSpace(os.Getenv("DV_BUILDER")); env != "" {
			opts.Builder = env
		}
	}
	useClassic := opts.ForceClassic || isTruthyEnv("DV_DISABLE_BUILDX")
	buildxOK := buildxAvailable()
	if !useClassic && buildxOK {
		return runBuildx(tag, dockerfilePath, contextDir, opts)
	}
	if !opts.ForceClassic && !buildxOK {
		if err := buildxError(); err != nil {
			fmt.Fprintf(os.Stderr, "buildx unavailable (%v); falling back to 'docker build'.\n", err)
		} else {
			fmt.Fprintln(os.Stderr, "buildx unavailable; falling back to 'docker build'.")
		}
	}
	return runClassicBuild(tag, dockerfilePath, contextDir, opts.ExtraArgs)
}

func runClassicBuild(tag, dockerfilePath, contextDir string, args []string) error {
	argv := []string{"build", "-t", tag, "-f", dockerfilePath}
	argv = append(argv, args...)
	argv = append(argv, contextDir)
	if isTruthyEnv("DV_VERBOSE") {
		fmt.Fprintf(os.Stderr, "Running: docker %s\n", strings.Join(argv, " "))
	}
	cmd := exec.Command("docker", argv...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	return cmd.Run()
}

func runBuildx(tag, dockerfilePath, contextDir string, opts BuildOptions) error {
	argv := []string{"buildx", "build", "--load", "-t", tag, "-f", dockerfilePath}
	if builder := strings.TrimSpace(opts.Builder); builder != "" {
		argv = append(argv, "--builder", builder)
	}
	argv = append(argv, opts.ExtraArgs...)
	argv = append(argv, contextDir)
	if isTruthyEnv("DV_VERBOSE") {
		fmt.Fprintf(os.Stderr, "Running: docker %s\n", strings.Join(argv, " "))
	}
	cmd := exec.Command("docker", argv...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	return cmd.Run()
}

var (
	buildxOnce sync.Once
	buildxOK   bool
	buildxErr  error
)

func buildxAvailable() bool {
	buildxOnce.Do(func() {
		cmd := exec.Command("docker", "buildx", "version")
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		buildxErr = cmd.Run()
		buildxOK = buildxErr == nil
	})
	return buildxOK
}

func buildxError() error {
	buildxAvailable()
	return buildxErr
}

func isTruthyEnv(key string) bool {
	val := strings.TrimSpace(os.Getenv(key))
	switch strings.ToLower(val) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func ImageExists(tag string) bool {
	out, _ := exec.Command("bash", "-lc", "docker images -q "+shellEscape(tag)).Output()
	return strings.TrimSpace(string(out)) != ""
}

func RemoveImage(tag string) error {
	if isTruthyEnv("DV_VERBOSE") {
		fmt.Fprintf(os.Stderr, "Running: docker rmi %s\n", tag)
	}
	cmd := exec.Command("docker", "rmi", tag)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// RemoveImageQuiet removes an image, suppressing output and errors.
// Useful for cleanup where failure is acceptable.
func RemoveImageQuiet(tag string) error {
	if isTruthyEnv("DV_VERBOSE") {
		fmt.Fprintf(os.Stderr, "Running: docker rmi -f %s\n", tag)
	}
	cmd := exec.Command("docker", "rmi", "-f", tag)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run()
}

// TagImage applies a new tag to an existing image (docker tag src dst)
func TagImage(srcTag, dstTag string) error {
	if isTruthyEnv("DV_VERBOSE") {
		fmt.Fprintf(os.Stderr, "Running: docker tag %s %s\n", srcTag, dstTag)
	}
	cmd := exec.Command("docker", "tag", srcTag, dstTag)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func Start(name string) error {
	if isTruthyEnv("DV_VERBOSE") {
		fmt.Fprintf(os.Stderr, "Running: docker start %s\n", name)
	}
	cmd := exec.Command("docker", "start", name)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// ContainerIP returns the IP address of a running container on the default bridge network.
func ContainerIP(name string) (string, error) {
	out, err := exec.Command("docker", "inspect", name, "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}").Output()
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("container %s has no IP address", name)
	}
	return ip, nil
}

func RunDetached(name, workdir, image string, hostPort, containerPort int, labels map[string]string, envs map[string]string, extraHosts []string, sshAuthSock string) error {
	args := []string{"run", "-d",
		"--name", name,
		"-w", workdir,
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", hostPort, containerPort),
	}
	// hostSSHAuthSock tracks what SSH_AUTH_SOCK should be on the host for Docker to forward
	hostSSHAuthSock := sshAuthSock
	if sshAuthSock != "" {
		var mountPath string
		var socketSource string
		if runtime.GOOS == "darwin" {
			// On macOS, Docker Desktop/OrbStack provide a magic socket that forwards
			// the host's SSH agent. We always mount this path.
			mountPath = "/run/host-services/ssh-auth.sock"

			// Check if there's an IdentityAgent configured (e.g., 1Password).
			// If so, we need to tell Docker to use that socket instead of SSH_AUTH_SOCK.
			identityAgent := getIdentityAgent()
			if identityAgent != "" {
				hostSSHAuthSock = identityAgent
				socketSource = "IdentityAgent from ~/.ssh/config"
			} else {
				socketSource = "SSH_AUTH_SOCK"
			}
		} else {
			// On Linux, mount the host socket directly
			mountPath = sshAuthSock
			socketSource = "SSH_AUTH_SOCK"
		}
		if isTruthyEnv("DV_VERBOSE") {
			fmt.Fprintf(os.Stderr, "SSH agent: forwarding %s (%s)\n", hostSSHAuthSock, socketSource)
			// Check if socket exists on host
			if _, err := os.Stat(hostSSHAuthSock); err != nil {
				fmt.Fprintf(os.Stderr, "SSH agent: WARNING - socket does not exist: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "SSH agent: socket exists at %s\n", hostSSHAuthSock)
			}
		}
		args = append(args, "-v", mountPath+":/tmp/ssh-agent.sock")
		args = append(args, "-e", "SSH_AUTH_SOCK=/tmp/ssh-agent.sock")
	}
	// Apply extra hosts
	for _, h := range extraHosts {
		args = append(args, "--add-host", h)
	}
	// Apply environment variables
	for k, v := range envs {
		if strings.TrimSpace(k) == "" || strings.Contains(k, "\n") {
			continue
		}
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	// Apply labels for provenance and discovery
	for k, v := range labels {
		if strings.TrimSpace(k) == "" || strings.Contains(k, "\n") {
			continue
		}
		args = append(args, "--label", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, image, "--sysctl", "kernel.unprivileged_userns_clone=1")
	if isTruthyEnv("DV_VERBOSE") {
		fmt.Fprintf(os.Stderr, "Running: docker %s\n", strings.Join(args, " "))
	}
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// If we detected a different SSH agent (e.g., 1Password), set SSH_AUTH_SOCK
	// in the docker command's environment so Docker Desktop/OrbStack forwards it
	if hostSSHAuthSock != "" && hostSSHAuthSock != sshAuthSock {
		// Filter out existing SSH_AUTH_SOCK and replace with our value
		env := os.Environ()
		filteredEnv := make([]string, 0, len(env))
		for _, e := range env {
			if !strings.HasPrefix(e, "SSH_AUTH_SOCK=") {
				filteredEnv = append(filteredEnv, e)
			}
		}
		cmd.Env = append(filteredEnv, "SSH_AUTH_SOCK="+hostSSHAuthSock)
		if isTruthyEnv("DV_VERBOSE") {
			fmt.Fprintf(os.Stderr, "SSH agent: setting SSH_AUTH_SOCK=%s for docker command\n", hostSSHAuthSock)
		}
	}
	return cmd.Run()
}

func ExecInteractive(name, workdir string, envs Envs, argv []string) error {
	args := []string{"exec", "-i", "--user", "discourse", "-w", workdir}
	// Add -t only when both stdin and stdout are TTYs
	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		args = append([]string{"exec", "-t"}, args[1:]...)
	}
	for _, e := range envs {
		args = append(args, "-e", e)
	}
	args = append(args, name)
	args = append(args, argv...)
	cmd := exec.Command("docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// ExecInteractiveAsRoot runs an interactive command inside the container as root.
func ExecInteractiveAsRoot(name, workdir string, envs Envs, argv []string) error {
	args := []string{"exec", "-i", "--user", "root", "-w", workdir}
	// Add -t only when both stdin and stdout are TTYs
	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		args = append([]string{"exec", "-t"}, args[1:]...)
	}
	for _, e := range envs {
		args = append(args, "-e", e)
	}
	args = append(args, name)
	args = append(args, argv...)
	cmd := exec.Command("docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// Envs is a typed slice for container environment variables.
// Using a distinct type prevents accidental argument swaps with argv.
type Envs []string

// ExecOutput runs a command inside the container as the discourse user.
// Use nil for envs when no environment variables are needed.
// Returns stdout only; use ExecCombinedOutput if you need stderr too.
func ExecOutput(name, workdir string, envs Envs, argv []string) (string, error) {
	args := []string{"exec", "--user", "discourse", "-w", workdir}
	for _, e := range envs {
		args = append(args, "-e", e)
	}
	args = append(args, name)
	args = append(args, argv...)
	cmd := exec.Command("docker", args...)
	out, err := cmd.Output()
	return string(out), err
}

// ExecOutputContext runs a command inside the container as the discourse user with context.
// Use nil for envs when no environment variables are needed.
// Returns stdout only; use ExecCombinedOutputContext if you need stderr too.
func ExecOutputContext(ctx context.Context, name, workdir string, envs Envs, argv []string) (string, error) {
	args := []string{"exec", "--user", "discourse", "-w", workdir}
	for _, e := range envs {
		args = append(args, "-e", e)
	}
	args = append(args, name)
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.Output()
	return string(out), err
}

// ExecCombinedOutput runs a command inside the container as the discourse user.
// Use nil for envs when no environment variables are needed.
// Returns both stdout and stderr combined.
func ExecCombinedOutput(name, workdir string, envs Envs, argv []string) (string, error) {
	args := []string{"exec", "--user", "discourse", "-w", workdir}
	for _, e := range envs {
		args = append(args, "-e", e)
	}
	args = append(args, name)
	args = append(args, argv...)
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ExecCombinedOutputContext runs a command inside the container as the discourse user with context.
// Use nil for envs when no environment variables are needed.
// Returns both stdout and stderr combined.
func ExecCombinedOutputContext(ctx context.Context, name, workdir string, envs Envs, argv []string) (string, error) {
	args := []string{"exec", "--user", "discourse", "-w", workdir}
	for _, e := range envs {
		args = append(args, "-e", e)
	}
	args = append(args, name)
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ExecAsRoot runs a command inside the container as root, returning output.
// Use nil for envs when no environment variables are needed.
// Returns stdout only; use ExecAsRootCombined if you need stderr too.
func ExecAsRoot(name, workdir string, envs Envs, argv []string) (string, error) {
	args := []string{"exec", "--user", "root", "-w", workdir}
	for _, e := range envs {
		args = append(args, "-e", e)
	}
	args = append(args, name)
	args = append(args, argv...)
	cmd := exec.Command("docker", args...)
	out, err := cmd.Output()
	return string(out), err
}

// ExecAsRootContext runs a command inside the container as root with context, returning output.
// Use nil for envs when no environment variables are needed.
// Returns stdout only; use ExecAsRootCombinedContext if you need stderr too.
func ExecAsRootContext(ctx context.Context, name, workdir string, envs Envs, argv []string) (string, error) {
	args := []string{"exec", "--user", "root", "-w", workdir}
	for _, e := range envs {
		args = append(args, "-e", e)
	}
	args = append(args, name)
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.Output()
	return string(out), err
}

// ExecAsRootCombined runs a command inside the container as root.
// Use nil for envs when no environment variables are needed.
// Returns both stdout and stderr combined.
func ExecAsRootCombined(name, workdir string, envs Envs, argv []string) (string, error) {
	args := []string{"exec", "--user", "root", "-w", workdir}
	for _, e := range envs {
		args = append(args, "-e", e)
	}
	args = append(args, name)
	args = append(args, argv...)
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ExecAsRootCombinedContext runs a command inside the container as root with context.
// Use nil for envs when no environment variables are needed.
// Returns both stdout and stderr combined.
func ExecAsRootCombinedContext(ctx context.Context, name, workdir string, envs Envs, argv []string) (string, error) {
	args := []string{"exec", "--user", "root", "-w", workdir}
	for _, e := range envs {
		args = append(args, "-e", e)
	}
	args = append(args, name)
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ExpandGlobInContainer runs a shell command to expand a glob pattern inside the container.
// Returns a list of matching paths, or an empty slice if no matches.
// The pattern can include ~ for the user's home directory and glob metacharacters (* ? [ {).
func ExpandGlobInContainer(containerName, pattern string) ([]string, error) {
	// Pass pattern as a positional argument to avoid command injection.
	// The script expands ~ to $HOME, enables nullglob to handle no-match gracefully,
	// and outputs one existing file per line.
	cmd := exec.Command("docker", "exec", containerName, "bash", "-c",
		`pattern=$1; pattern=${pattern/#\~/$HOME}; shopt -s nullglob; for f in $pattern; do [ -e "$f" ] && echo "$f"; done`,
		"--", pattern)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var paths []string
	for _, line := range lines {
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

// ContainsGlobMeta returns true if the path contains glob metacharacters (* ? [ {).
// Also detects brace expansion patterns like {a,b}.
func ContainsGlobMeta(path string) bool {
	return strings.ContainsAny(path, "*?[{")
}

func CopyFromContainer(name, srcInContainer, dstOnHost string) error {
	cmd := exec.Command("docker", "cp", fmt.Sprintf("%s:%s", name, srcInContainer), dstOnHost)
	if isTruthyEnv("DV_VERBOSE") {
		cmd.Stdout = os.Stdout
	} else {
		cmd.Stdout = io.Discard
	}
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func CopyFromContainerContext(ctx context.Context, name, srcInContainer, dstOnHost string) error {
	cmd := exec.CommandContext(ctx, "docker", "cp", fmt.Sprintf("%s:%s", name, srcInContainer), dstOnHost)
	if isTruthyEnv("DV_VERBOSE") {
		cmd.Stdout = os.Stdout
	} else {
		cmd.Stdout = io.Discard
	}
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func CopyToContainer(name, srcOnHost, dstInContainer string) error {
	cmd := exec.Command("docker", "cp", srcOnHost, fmt.Sprintf("%s:%s", name, dstInContainer))
	if isTruthyEnv("DV_VERBOSE") {
		cmd.Stdout = os.Stdout
	} else {
		cmd.Stdout = io.Discard
	}
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func CopyToContainerContext(ctx context.Context, name, srcOnHost, dstInContainer string) error {
	cmd := exec.CommandContext(ctx, "docker", "cp", srcOnHost, fmt.Sprintf("%s:%s", name, dstInContainer))
	if isTruthyEnv("DV_VERBOSE") {
		cmd.Stdout = os.Stdout
	} else {
		cmd.Stdout = io.Discard
	}
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// CopyToContainerWithOwnership copies a file or directory into a container and
// sets its ownership to discourse:discourse. If recursive is true, ownership is
// set recursively (useful for directories).
func CopyToContainerWithOwnership(name, srcOnHost, dstInContainer string, recursive bool) error {
	if err := CopyToContainer(name, srcOnHost, dstInContainer); err != nil {
		return err
	}

	chownArgs := []string{"chown"}
	if recursive {
		chownArgs = append(chownArgs, "-R")
	}
	chownArgs = append(chownArgs, "discourse:discourse", dstInContainer)

	if _, err := ExecAsRoot(name, "/", nil, chownArgs); err != nil {
		return fmt.Errorf("failed to set ownership on %s: %w", dstInContainer, err)
	}
	return nil
}

// CopyToContainerWithOwnershipContext copies a file or directory into a container with context
// and sets its ownership to discourse:discourse. If recursive is true, ownership is set recursively.
func CopyToContainerWithOwnershipContext(ctx context.Context, name, srcOnHost, dstInContainer string, recursive bool) error {
	if err := CopyToContainerContext(ctx, name, srcOnHost, dstInContainer); err != nil {
		return err
	}

	chownArgs := []string{"chown"}
	if recursive {
		chownArgs = append(chownArgs, "-R")
	}
	chownArgs = append(chownArgs, "discourse:discourse", dstInContainer)

	if _, err := ExecAsRootContext(ctx, name, "/", nil, chownArgs); err != nil {
		return fmt.Errorf("failed to set ownership on %s: %w", dstInContainer, err)
	}
	return nil
}

func shellEscape(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		if r == '\\' || r == '"' || r == '$' || r == '`' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func Labels(name string) (map[string]string, error) {
	cmd := exec.Command("docker", "inspect", "-f", "{{json .Config.Labels}}", name)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	if err := json.Unmarshal(out, &labels); err != nil {
		return nil, err
	}
	if labels == nil {
		labels = map[string]string{}
	}
	return labels, nil
}

func UpdateLabels(name string, labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}
	args := []string{"update"}
	for k, v := range labels {
		args = append(args, "--label-add", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, name)
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// GetContainerHostPort returns the host port mapped to the given container port.
// Returns 0 if no mapping found or container doesn't exist.
// Works on both running and stopped containers by inspecting HostConfig.
func GetContainerHostPort(name string, containerPort int) (int, error) {
	// Use docker inspect to get port bindings - works even when container is stopped
	portKey := fmt.Sprintf("%d/tcp", containerPort)
	format := fmt.Sprintf("{{(index .HostConfig.PortBindings \"%s\" 0).HostPort}}", portKey)
	out, err := exec.Command("docker", "inspect", "-f", format, name).Output()
	if err != nil {
		return 0, err
	}
	portStr := strings.TrimSpace(string(out))
	if portStr == "" || portStr == "<no value>" {
		return 0, fmt.Errorf("no port mapping found")
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return 0, fmt.Errorf("invalid port number: %s", portStr)
	}
	return port, nil
}

// CommitContainer creates an image from a container's current filesystem state.
func CommitContainer(name, imageTag string) error {
	cmd := exec.Command("docker", "commit", name, imageTag)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// AllocatedPorts returns a set of all host ports currently allocated by Docker
// containers (running or stopped). It uses a more robust approach by listing
// all containers and inspecting them individually to avoid failing on a single
// malformed container.
func AllocatedPorts() (map[int]bool, error) {
	// 1. Get all container IDs
	out, err := exec.Command("docker", "ps", "-aq").Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return make(map[int]bool), nil
	}

	// 2. Inspect all containers at once with a template that handles multiple ports
	format := "{{range $p, $conf := .HostConfig.PortBindings}}{{(index $conf 0).HostPort}} {{end}}"
	args := append([]string{"inspect", "-f", format}, ids...)
	out, err = exec.Command("docker", args...).Output()
	if err != nil {
		// If batch inspect fails, fallback to one-by-one to be resilient
		return allocatedPortsOneByOne(ids)
	}

	ports := make(map[int]bool)
	fields := strings.Fields(string(out))
	for _, f := range fields {
		var p int
		if _, err := fmt.Sscanf(f, "%d", &p); err == nil {
			ports[p] = true
		}
	}
	return ports, nil
}

func allocatedPortsOneByOne(ids []string) (map[int]bool, error) {
	ports := make(map[int]bool)
	format := "{{range $p, $conf := .HostConfig.PortBindings}}{{(index $conf 0).HostPort}} {{end}}"
	for _, id := range ids {
		out, err := exec.Command("docker", "inspect", "-f", format, id).Output()
		if err != nil {
			continue // skip malformed or missing containers
		}
		fields := strings.Fields(string(out))
		for _, f := range fields {
			var p int
			if _, err := fmt.Sscanf(f, "%d", &p); err == nil {
				ports[p] = true
			}
		}
	}
	return ports, nil
}

// GetContainerWorkdir returns the working directory configured for a container.
func GetContainerWorkdir(name string) (string, error) {
	out, err := exec.Command("docker", "inspect", "-f", "{{.Config.WorkingDir}}", name).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// TopProcess represents a single process from docker top output.
type TopProcess struct {
	PID  int
	PPID int
	User string
	Args string
}

// ExecSession represents a docker exec'd process detected via orphan-PPID analysis.
type ExecSession struct {
	PID     int
	User    string
	Command string
}

// TopProcesses runs `docker top <name> -o pid,ppid,user,args` and parses the output.
func TopProcesses(name string) ([]TopProcess, error) {
	cmd := exec.Command("docker", "top", name, "-o", "pid,ppid,user,args")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker top %s: %w", name, err)
	}
	return ParseTopOutput(string(out))
}

// ParseTopOutput parses the text output of `docker top` with columns pid,ppid,user,args.
func ParseTopOutput(output string) ([]TopProcess, error) {
	var procs []TopProcess
	for i, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || i == 0 { // skip header
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		procs = append(procs, TopProcess{
			PID:  pid,
			PPID: ppid,
			User: fields[2],
			Args: strings.Join(fields[3:], " "),
		})
	}
	return procs, nil
}

// containerInitPID returns the host PID of the container's init process
// via `docker inspect`.
func containerInitPID(name string) (int, error) {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Pid}}", name).Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// ExecSessions detects docker exec'd processes by finding processes whose PPID
// does not belong to any other process inside the container (orphan-PPID detection).
// The container's init process is excluded since it also has an external PPID.
// docker top shows host PIDs, so we use docker inspect to find the init PID.
func ExecSessions(name string) ([]ExecSession, error) {
	procs, err := TopProcesses(name)
	if err != nil {
		return nil, err
	}

	initPID, err := containerInitPID(name)
	if err != nil {
		return nil, fmt.Errorf("cannot determine container init PID for %s: %w", name, err)
	}

	return FindExecSessions(procs, initPID), nil
}

// FindExecSessions filters a process list for orphan-PPID entries, excluding initPID.
// A process has an "orphan PPID" when its PPID doesn't match any other PID in the list,
// meaning its parent lives outside the container (containerd-shim for docker exec).
func FindExecSessions(procs []TopProcess, initPID int) []ExecSession {
	pids := make(map[int]bool, len(procs))
	for _, p := range procs {
		pids[p.PID] = true
	}

	var sessions []ExecSession
	for _, p := range procs {
		if p.PID == initPID {
			continue
		}
		if !pids[p.PPID] {
			sessions = append(sessions, ExecSession{
				PID:     p.PID,
				User:    p.User,
				Command: p.Args,
			})
		}
	}
	return sessions
}

// GetContainerEnv returns environment variables set on a container as a map.
func GetContainerEnv(name string) (map[string]string, error) {
	out, err := exec.Command("docker", "inspect", "-f", "{{json .Config.Env}}", name).Output()
	if err != nil {
		return nil, err
	}
	var envList []string
	if err := json.Unmarshal(out, &envList); err != nil {
		return nil, err
	}
	envMap := make(map[string]string)
	for _, e := range envList {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}
	return envMap, nil
}
