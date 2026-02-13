package localproxy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"dv/internal/assets"
	"dv/internal/config"
	"dv/internal/docker"
)

func BuildImage(configDir string, cfg config.LocalProxyConfig) error {
	dockerfile, contextDir, err := assets.MaterializeLocalProxyContext(configDir)
	if err != nil {
		return err
	}
	return docker.BuildFrom(cfg.ImageTag, dockerfile, contextDir, docker.BuildOptions{})
}

func EnsureContainer(configDir string, cfg config.LocalProxyConfig, recreate bool) error {
	name := strings.TrimSpace(cfg.ContainerName)
	if name == "" {
		return fmt.Errorf("local proxy container name is empty")
	}

	if cfg.HTTPPort == cfg.APIPort {
		return fmt.Errorf("http and api ports must differ")
	}
	if cfg.HTTPS && cfg.HTTPSPort == cfg.APIPort {
		return fmt.Errorf("https and api ports must differ")
	}
	if cfg.HTTPS && cfg.HTTPSPort == cfg.HTTPPort {
		return fmt.Errorf("https and http ports must differ")
	}

	if recreate && docker.Exists(name) {
		_ = docker.Stop(name)
		_ = docker.Remove(name)
	}

	if docker.Exists(name) {
		// Ensure restart policy is set (best effort)
		updateRestartPolicy(name)
		if docker.Running(name) {
			return nil
		}
		return docker.Start(name)
	}

	if PortOccupied(cfg.HTTPPort) {
		return fmt.Errorf("host port %d is already in use", cfg.HTTPPort)
	}
	if cfg.HTTPS && PortOccupied(cfg.HTTPSPort) {
		return fmt.Errorf("host port %d is already in use", cfg.HTTPSPort)
	}
	if PortOccupied(cfg.APIPort) {
		return fmt.Errorf("host port %d is already in use", cfg.APIPort)
	}

	args := []string{
		"run", "-d",
		"--name", name,
	}

	// Bind to appropriate interface based on public flag
	if cfg.Public {
		args = append(args, "-p", fmt.Sprintf("%d:%d", cfg.HTTPPort, 80))
		if cfg.HTTPS {
			args = append(args, "-p", fmt.Sprintf("%d:%d", cfg.HTTPSPort, 443))
		}
		args = append(args, "-p", fmt.Sprintf("%d:%d", cfg.APIPort, 2080))
	} else {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", cfg.HTTPPort, 80))
		if cfg.HTTPS {
			args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", cfg.HTTPSPort, 443))
		}
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", cfg.APIPort, 2080))
	}

	args = append(args,
		"--add-host", "host.docker.internal:host-gateway",
		"--restart", "unless-stopped",
		"--label", "com.dv.owner=dv",
		"--label", LabelEnabled+"=true",
		"--label", LabelHTTPPort+"="+strconv.Itoa(cfg.HTTPPort),
	)
	if cfg.HTTPS {
		args = append(args, "--label", LabelHTTPSPort+"="+strconv.Itoa(cfg.HTTPSPort))
	}

	args = append(args, "-e", "PROXY_HTTP_ADDR=:80")
	args = append(args, "-e", "PROXY_API_ADDR=:2080")
	args = append(args, "-e", "PROXY_HOSTNAME_SUFFIX="+cfg.Hostname)

	dockerSocketSource := detectDockerSocketSource()
	if dockerSocketSource != "" {
		args = append(args, "-v", fmt.Sprintf("%s:/var/run/docker.sock", dockerSocketSource))
		args = append(args, "-e", "PROXY_DOCKER_SOCKET=/var/run/docker.sock")
		args = append(args, "-e", "PROXY_AUTO_HEAL=1")
	} else {
		// Graceful degradation: run proxy as before, but disable auto-heal.
		args = append(args, "-e", "PROXY_AUTO_HEAL=0")
	}
	if cfg.HTTPS {
		certPath, keyPath := TLSPaths(configDir)
		if !fileNonEmpty(certPath) || !fileNonEmpty(keyPath) {
			return fmt.Errorf("missing TLS cert/key at %s and %s (run dv config local-proxy --https to generate them)", certPath, keyPath)
		}
		tlsDir := filepath.Dir(certPath)
		args = append(args, "-v", fmt.Sprintf("%s:/etc/local-proxy/tls:ro", tlsDir))
		args = append(args, "-e", "PROXY_HTTPS_ADDR=:443")
		args = append(args, "-e", "PROXY_TLS_CERT_FILE=/etc/local-proxy/tls/"+filepath.Base(certPath))
		args = append(args, "-e", "PROXY_TLS_KEY_FILE=/etc/local-proxy/tls/"+filepath.Base(keyPath))
		args = append(args, "-e", "PROXY_EXTERNAL_HTTPS_PORT="+strconv.Itoa(cfg.HTTPSPort))
		args = append(args, "-e", "PROXY_REDIRECT_HTTP_TO_HTTPS=1")
	}

	args = append(args, cfg.ImageTag)

	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func updateRestartPolicy(name string) {
	cmd := exec.Command("docker", "update", "--restart", "unless-stopped", name)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	_ = cmd.Run()
}

func detectDockerSocketSource() string {
	homeDir, _ := os.UserHomeDir()
	return detectDockerSocketSourceWith(strings.TrimSpace(os.Getenv("DOCKER_HOST")), homeDir, socketPathExists)
}

func detectDockerSocketSourceWith(dockerHost string, homeDir string, exists func(string) bool) string {
	if exists == nil {
		return ""
	}
	dockerHost = strings.TrimSpace(dockerHost)
	if dockerHost != "" {
		if strings.HasPrefix(dockerHost, "unix://") {
			path := strings.TrimSpace(strings.TrimPrefix(dockerHost, "unix://"))
			if path != "" && exists(path) {
				return path
			}
		} else {
			// Non-unix Docker endpoints (tcp/ssh/etc.) are not mountable into the
			// local proxy container, so disable auto-heal.
			return ""
		}
	}

	candidates := []string{"/var/run/docker.sock"}
	if trimmedHome := strings.TrimSpace(homeDir); trimmedHome != "" {
		// Docker Desktop for macOS commonly exposes a user-level socket here.
		candidates = append(candidates, filepath.Join(trimmedHome, ".docker", "run", "docker.sock"))
		// OrbStack commonly exposes a user-level socket here.
		candidates = append(candidates, filepath.Join(trimmedHome, ".orbstack", "run", "docker.sock"))
	}
	for _, candidate := range candidates {
		if exists(candidate) {
			return candidate
		}
	}
	return ""
}

func socketPathExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
