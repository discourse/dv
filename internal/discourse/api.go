// Package discourse provides a centralized HTTP client for the Discourse Admin API
// with automatic API key generation, caching, and recovery.
package discourse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dv/internal/config"
	"dv/internal/docker"
)

const (
	// APIKeyDescription is the description used when creating API keys
	APIKeyDescription = "dv-api-client"
	// ContainerKeyPath is where the API key is stored inside the container
	ContainerKeyPath = "/home/discourse/.dv/api_key"
	// DefaultTimeout for HTTP requests
	DefaultTimeout = 30 * time.Second
)

// Client provides HTTP-based access to Discourse Admin APIs
type Client struct {
	BaseURL       string
	APIKey        string
	APIUsername   string
	ContainerName string
	Workdir       string
	Verbose       bool
	Envs          docker.Envs // Environment variables for container execution
	httpClient    *http.Client

	// hostKeyCache is the path to the host-side key cache file
	hostKeyCache string
}

// KeyCache represents the host-side API key cache
type KeyCache struct {
	Keys map[string]KeyEntry `json:"keys"`
}

// KeyEntry is a single cached API key
type KeyEntry struct {
	APIKey    string    `json:"api_key"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

// NewClient creates a new Discourse API client for the given container.
// It automatically discovers the base URL and loads cached credentials.
func NewClient(containerName string, cfg config.Config, envs docker.Envs, verbose bool) (*Client, error) {
	imgCfg := cfg.Images[cfg.SelectedImage]
	workdir := config.EffectiveWorkdir(cfg, imgCfg, containerName)

	baseURL, err := DiscoverBaseURL(containerName, cfg)
	if err != nil {
		return nil, fmt.Errorf("discover base URL: %w", err)
	}

	c := &Client{
		BaseURL:       baseURL,
		ContainerName: containerName,
		Workdir:       workdir,
		Verbose:       verbose,
		Envs:          envs,
		httpClient: &http.Client{
			Timeout: DefaultTimeout,
		},
	}

	return c, nil
}

// NewClientWithURL creates a client with an explicit base URL (for testing or custom setups)
func NewClientWithURL(containerName, baseURL, workdir string, envs docker.Envs, verbose bool) *Client {
	return &Client{
		BaseURL:       baseURL,
		ContainerName: containerName,
		Workdir:       workdir,
		Verbose:       verbose,
		Envs:          envs,
		httpClient: &http.Client{
			Timeout: DefaultTimeout,
		},
	}
}

// EnsureAPIKey ensures we have a valid API key, generating one if needed.
// This is the main entry point for key management.
func (c *Client) EnsureAPIKey() error {
	// Step 1: Try loading from container file
	if err := c.loadKeyFromContainer(); err == nil {
		// Step 2: Verify the key works
		if err := c.testConnection(); err == nil {
			return nil
		}
		c.verboseLog("Cached key invalid, regenerating...")
	}

	// Step 3: Generate new key via Rails
	if err := c.generateKey(); err != nil {
		return fmt.Errorf("generate API key: %w", err)
	}

	// Step 4: Verify the new key works
	if err := c.testConnection(); err != nil {
		return fmt.Errorf("verify new key: %w", err)
	}

	return nil
}

// GetAPIKey returns the current API key and username, ensuring one exists.
func (c *Client) GetAPIKey() (apiKey, username string, err error) {
	if c.APIKey == "" {
		if err := c.EnsureAPIKey(); err != nil {
			return "", "", err
		}
	}
	return c.APIKey, c.APIUsername, nil
}

// loadKeyFromContainer reads the cached API key from the container filesystem
func (c *Client) loadKeyFromContainer() error {
	if !docker.Running(c.ContainerName) {
		return fmt.Errorf("container not running")
	}

	readCmd := fmt.Sprintf("cat %s 2>/dev/null", shellQuote(ContainerKeyPath))
	out, err := docker.ExecOutput(c.ContainerName, c.Workdir, c.Envs, []string{"bash", "-c", readCmd})
	if err != nil {
		return fmt.Errorf("read key file: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return fmt.Errorf("invalid key file format")
	}

	c.APIKey = strings.TrimSpace(lines[0])
	c.APIUsername = strings.TrimSpace(lines[1])

	if c.APIKey == "" || c.APIUsername == "" {
		return fmt.Errorf("empty key or username")
	}

	c.verboseLog("Loaded API key from container cache")
	return nil
}

// generateKey creates a new API key via Rails runner and saves it to the container
func (c *Client) generateKey() error {
	if !docker.Running(c.ContainerName) {
		return fmt.Errorf("container %s not running - run 'dv start' first", c.ContainerName)
	}

	generated, err := GenerateAPIKey(GenerateAPIKeyOptions{
		ContainerName: c.ContainerName,
		Workdir:       c.Workdir,
		Description:   APIKeyDescription,
		Envs:          c.Envs,
		Verbose:       c.Verbose,
	})
	if err != nil {
		return err
	}

	c.APIKey = generated.Key
	c.APIUsername = generated.Username

	// Save to container file
	if err := c.saveKeyToContainer(); err != nil {
		c.verboseLog("Warning: failed to cache key: %v", err)
		// Non-fatal, we can still use the key
	}

	c.verboseLog("Generated new API key for user %s", c.APIUsername)
	return nil
}

// saveKeyToContainer writes the API key to the container filesystem for caching
func (c *Client) saveKeyToContainer() error {
	content := fmt.Sprintf("%s\n%s\n", c.APIKey, c.APIUsername)
	saveCmd := fmt.Sprintf(
		"install -d -m 700 %s && printf '%%s' %s > %s && chmod 600 %s",
		shellQuote("/home/discourse/.dv"),
		shellQuote(content),
		shellQuote(ContainerKeyPath),
		shellQuote(ContainerKeyPath),
	)
	_, err := docker.ExecOutput(c.ContainerName, c.Workdir, c.Envs, []string{"bash", "-c", saveCmd})
	return err
}

// testConnection verifies the API key works by making a simple request.
// Keep this independent of plugin-provided settings; during early boot or after
// plugin changes, probing an AI-specific site setting can fail with a Rails 500
// even when the generated API key is valid.
func (c *Client) testConnection() error {
	if c.APIKey == "" {
		return fmt.Errorf("no API key set")
	}

	req, err := http.NewRequest("GET", c.BaseURL+"/session/current.json", nil)
	if err != nil {
		return err
	}
	c.setAuthHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fmt.Errorf("authentication failed (status %d)", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		c.verboseLog("API key verification got server status %d; assuming key is usable and continuing: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		return nil
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("unexpected status: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

// setAuthHeaders adds the required API authentication headers
func (c *Client) setAuthHeaders(req *http.Request) {
	req.Header.Set("Api-Key", c.APIKey)
	req.Header.Set("Api-Username", c.APIUsername)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
}

// doRequest performs an HTTP request with automatic key recovery on auth failure
func (c *Client) doRequest(method, path string, body interface{}) (*http.Response, []byte, error) {
	// Ensure we have a key
	if c.APIKey == "" {
		if err := c.EnsureAPIKey(); err != nil {
			return nil, nil, err
		}
	}

	resp, respBody, err := c.doRequestOnce(method, path, body)
	if err != nil {
		return nil, nil, err
	}

	// Auto-recovery on auth failure
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		c.verboseLog("Auth failed, regenerating key...")
		if err := c.generateKey(); err != nil {
			return nil, nil, fmt.Errorf("key regeneration failed: %w", err)
		}
		// Retry once with new key
		return c.doRequestOnce(method, path, body)
	}

	return resp, respBody, nil
}

// doRequestOnce performs a single HTTP request without retry
func (c *Client) doRequestOnce(method, path string, body interface{}) (*http.Response, []byte, error) {
	var bodyReader io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(jsonBody)
	}

	url := c.BaseURL + path
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, nil, err
	}
	c.setAuthHeaders(req)

	c.verboseLog("%s %s", method, url)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read response: %w", err)
	}

	return resp, respBody, nil
}

// verboseLog prints debug output when verbose mode is enabled
func (c *Client) verboseLog(format string, args ...interface{}) {
	if c.Verbose {
		fmt.Printf("[discourse-api] "+format+"\n", args...)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// DiscoverBaseURL determines the correct URL to reach a container's Discourse instance
func DiscoverBaseURL(containerName string, cfg config.Config) (string, error) {
	// Option 1: Local proxy is enabled - use NAME.hostname
	if cfg.LocalProxy.Enabled {
		host := hostnameForContainer(containerName, cfg.LocalProxy.Hostname)
		port := cfg.LocalProxy.HTTPPort
		if port == 80 {
			return fmt.Sprintf("http://%s", host), nil
		}
		return fmt.Sprintf("http://%s:%d", host, port), nil
	}

	// Option 2: Parse docker port mapping
	port, err := getContainerHostPort(containerName)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://localhost:%d", port), nil
}

// hostnameForContainer converts a container name to a valid hostname using the configured domain
func hostnameForContainer(name, hostname string) string {
	base := strings.ToLower(strings.TrimSpace(name))
	base = strings.ReplaceAll(base, "_", "-")
	re := regexp.MustCompile(`[^a-z0-9-]`)
	base = re.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-.")
	if base == "" {
		base = "dv"
	}
	if hostname == "" {
		hostname = "dv.localhost"
	}
	return base + "." + hostname
}

// getContainerHostPort extracts the published host port from a running container
func getContainerHostPort(containerName string) (int, error) {
	// Use docker port command to get the mapping
	cmd := exec.Command("docker", "port", containerName)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("get container port: %w", err)
	}

	// Parse output like "3000/tcp -> 0.0.0.0:3001"
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Find the host port after the last colon
		arrowIdx := strings.Index(line, "->")
		if arrowIdx == -1 {
			continue
		}
		right := strings.TrimSpace(line[arrowIdx+2:])
		colonIdx := strings.LastIndex(right, ":")
		if colonIdx == -1 {
			continue
		}
		portStr := right[colonIdx+1:]
		port, err := strconv.Atoi(portStr)
		if err == nil && port > 0 {
			return port, nil
		}
	}

	return 0, fmt.Errorf("no published port found for container %s", containerName)
}
