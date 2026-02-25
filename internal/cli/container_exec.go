package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

type containerExecContext struct {
	name    string
	workdir string
	envs    docker.Envs
}

func prepareContainerExecContext(cmd *cobra.Command, overrideName ...string) (containerExecContext, bool, error) {
	configDir, err := xdg.ConfigDir()
	if err != nil {
		return containerExecContext{}, false, err
	}
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		return containerExecContext{}, false, err
	}

	name := ""
	if len(overrideName) > 0 && overrideName[0] != "" {
		name = overrideName[0]
	} else {
		name, _ = cmd.Flags().GetString("name")
		if name == "" {
			name = currentAgentName(cfg)
		}
	}
	if strings.TrimSpace(name) == "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "No container selected. Run 'dv start' first.")
		return containerExecContext{}, false, nil
	}

	if !docker.Exists(name) {
		fmt.Fprintf(cmd.OutOrStdout(), "Container '%s' does not exist. Run 'dv start' first.\n", name)
		return containerExecContext{}, false, nil
	}
	if !docker.Running(name) {
		fmt.Fprintf(cmd.OutOrStdout(), "Starting container '%s'...\n", name)
		if err := docker.Start(name); err != nil {
			return containerExecContext{}, false, err
		}
	}

	imgName := cfg.ContainerImages[name]
	var imgCfg config.ImageConfig
	if imgName != "" {
		imgCfg = cfg.Images[imgName]
	} else {
		_, imgCfg, err = resolveImage(cfg, "")
		if err != nil {
			return containerExecContext{}, false, err
		}
	}
	workdir := config.EffectiveWorkdir(cfg, imgCfg, name)

	copyConfiguredFiles(cmd, cfg, name, workdir, "")

	envs := collectEnvPassthrough(cfg)

	return containerExecContext{
		name:    name,
		workdir: workdir,
		envs:    envs,
	}, true, nil
}

func copyConfiguredFiles(cmd *cobra.Command, cfg config.Config, containerName, workdir, agent string) {
	agent = strings.ToLower(strings.TrimSpace(agent))
	for _, rule := range cfg.CopyRules {
		if !ruleMatchesAgent(rule, agent) {
			continue
		}
		hostPaths := expandHostSources(rule.Host)

		// Filter to existing files or directories
		type pathInfo struct {
			path  string
			isDir bool
		}
		var validPaths []pathInfo
		for _, hp := range hostPaths {
			if hp == "" {
				continue
			}
			st, err := os.Stat(hp)
			if err != nil {
				continue
			}
			if st.Mode().IsRegular() || st.IsDir() {
				validPaths = append(validPaths, pathInfo{path: hp, isDir: st.IsDir()})
			}
		}

		// If no valid paths and we have a fallback, try it
		if len(validPaths) == 0 && rule.Fallback != nil && rule.Fallback.Type == "command" {
			tmpPath, err := runFallbackCommand(rule.Fallback.Exec)
			if err == nil && tmpPath != "" {
				validPaths = []pathInfo{{path: tmpPath, isDir: false}}
				defer os.Remove(tmpPath)
			}
			// Silently skip if fallback also fails
		}

		for _, hp := range validPaths {
			target := containerPathFor(rule.Container, hp.path)

			// Skip if destination already exists in container
			if rule.SkipIfPresent {
				out, err := docker.ExecOutput(containerName, workdir, nil, []string{"test", "-e", target})
				if err == nil && out == "" {
					continue
				}
			}

			dstDir := filepath.Dir(target)
			_, _ = docker.ExecOutput(containerName, workdir, nil, []string{"bash", "-lc", "mkdir -p " + shellQuote(dstDir)})

			if len(rule.CopyKeys) > 0 && strings.HasSuffix(strings.ToLower(hp.path), ".json") {
				if err := copyJsonKeys(containerName, hp.path, target, rule.CopyKeys); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Failed to copy keys from %s to %s: %v\n", hp.path, target, err)
				}
				continue
			}

			if rule.MergeKey != "" && strings.HasSuffix(strings.ToLower(hp.path), ".json") {
				if err := mergeAndCopyJSON(containerName, hp.path, target, rule.MergeKey); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Failed to merge and copy %s to %s: %v\n", hp.path, target, err)
				}
				continue
			}

			if err := docker.CopyToContainerWithOwnership(containerName, hp.path, target, hp.isDir); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Failed to copy %s to %s: %v\n", hp.path, target, err)
				continue
			}
		}
	}
}

// runFallbackCommand executes a shell command and returns a temp file path containing stdout.
func runFallbackCommand(command string) (string, error) {
	cmd := exec.Command("sh", "-c", command)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	if len(output) == 0 {
		return "", fmt.Errorf("fallback command produced no output")
	}

	tmpFile, err := os.CreateTemp("", "dv-fallback-*")
	if err != nil {
		return "", err
	}
	if _, err := tmpFile.Write(output); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", err
	}
	tmpFile.Close()
	return tmpFile.Name(), nil
}

func copyJsonKeys(containerName, hostPath, target string, keys []string) error {
	hostData, err := os.ReadFile(hostPath)
	if err != nil {
		return err
	}

	var hostJSON map[string]any
	if err := json.Unmarshal(hostData, &hostJSON); err != nil {
		return fmt.Errorf("failed to parse host JSON %s: %w", hostPath, err)
	}

	// Extract only specified keys from host
	extracted := make(map[string]any)
	for _, key := range keys {
		if val, ok := hostJSON[key]; ok {
			extracted[key] = val
		}
	}

	// Nothing to copy if none of the keys exist in host
	if len(extracted) == 0 {
		return nil
	}

	tmpDir, err := os.MkdirTemp("", "dv-copy-keys-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	// Try to read existing container JSON, start with empty {} if missing
	containerJSON := make(map[string]any)
	containerTmp := filepath.Join(tmpDir, "container.json")
	if err := docker.CopyFromContainer(containerName, target, containerTmp); err == nil {
		containerData, err := os.ReadFile(containerTmp)
		if err == nil && len(containerData) > 0 {
			_ = json.Unmarshal(containerData, &containerJSON)
		}
	}

	// Merge extracted keys into container (host wins for specified keys)
	for k, v := range extracted {
		containerJSON[k] = v
	}

	mergedData, err := json.MarshalIndent(containerJSON, "", "  ")
	if err != nil {
		return err
	}

	mergedTmp := filepath.Join(tmpDir, "merged.json")
	if err := os.WriteFile(mergedTmp, mergedData, 0o644); err != nil {
		return err
	}

	return docker.CopyToContainerWithOwnership(containerName, mergedTmp, target, false)
}

func mergeAndCopyJSON(containerName, hostPath, target, mergeKey string) error {
	hostData, err := os.ReadFile(hostPath)
	if err != nil {
		return err
	}

	var hostJSON map[string]any
	if err := json.Unmarshal(hostData, &hostJSON); err != nil {
		return fmt.Errorf("failed to parse host JSON %s: %w", hostPath, err)
	}

	// Try to read existing container JSON
	tmpDir, err := os.MkdirTemp("", "dv-merge-json-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	hostTmp := filepath.Join(tmpDir, "container.json")
	var containerJSON map[string]any

	if err := docker.CopyFromContainer(containerName, target, hostTmp); err == nil {
		containerData, err := os.ReadFile(hostTmp)
		if err == nil && len(containerData) > 0 {
			_ = json.Unmarshal(containerData, &containerJSON)
		}
	}

	if containerJSON == nil {
		// No existing container JSON or failed to read, just copy host file
		return docker.CopyToContainerWithOwnership(containerName, hostPath, target, false)
	}

	// Perform merge: host wins for everything except mergeKey which is merged
	merged := deepMerge(containerJSON, hostJSON, mergeKey)

	mergedData, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}

	mergedTmp := filepath.Join(tmpDir, "merged.json")
	if err := os.WriteFile(mergedTmp, mergedData, 0o644); err != nil {
		return err
	}

	return docker.CopyToContainerWithOwnership(containerName, mergedTmp, target, false)
}

func deepMerge(dst, src map[string]any, mergeKey string) map[string]any {
	out := make(map[string]any)
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		if k == mergeKey {
			// Merge the specific key
			dstVal, dstOk := out[k].(map[string]any)
			srcVal, srcOk := v.(map[string]any)
			if dstOk && srcOk {
				mergedKey := make(map[string]any)
				for mk, mv := range dstVal {
					mergedKey[mk] = mv
				}
				for mk, mv := range srcVal {
					mergedKey[mk] = mv
				}
				out[k] = mergedKey
				continue
			}
		}
		// Host (src) wins for other keys or if types don't match for mergeKey
		out[k] = v
	}
	return out
}

func collectEnvPassthrough(cfg config.Config) docker.Envs {
	envs := make(docker.Envs, 0, len(cfg.EnvPassthrough)+len(cfg.Env)+1)
	for _, key := range cfg.EnvPassthrough {
		if val, ok := os.LookupEnv(key); ok && val != "" {
			envs = append(envs, key)
		}
	}
	for k, v := range cfg.Env {
		envs = append(envs, k+"="+v)
	}
	return envs
}

// expandHostPath expands a host path allowing ~ and environment variables.
func expandHostPath(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				p = home
			} else if strings.HasPrefix(p, "~/") {
				p = filepath.Join(home, p[2:])
			}
		}
	}
	p = os.ExpandEnv(p)
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func expandHostSources(p string) []string {
	expanded := expandHostPath(p)
	if strings.ContainsAny(expanded, "*?[") {
		matches, err := filepath.Glob(expanded)
		if err != nil {
			return nil
		}
		return matches
	}
	return []string{expanded}
}

// shellQuote is now in shared.go

func ruleMatchesAgent(rule config.CopyRule, agent string) bool {
	if len(rule.Agents) == 0 {
		return true
	}
	if agent == "" {
		return false
	}
	for _, a := range rule.Agents {
		if strings.EqualFold(strings.TrimSpace(a), agent) {
			return true
		}
	}
	return false
}

func containerPathFor(containerDst string, hostPath string) string {
	if strings.HasSuffix(containerDst, "/") {
		return path.Join(containerDst, filepath.Base(hostPath))
	}
	return containerDst
}
