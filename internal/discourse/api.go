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

	c.verboseLog("Generating new API key via Rails...")

	// Ruby script to create or find existing API key
	// Uses DV_API_KEY: and DV_USERNAME: markers to be robust against warnings/noise in stdout
	rubyScript := fmt.Sprintf(`
require "json"
ActiveRecord::Base.logger = nil
Rails.logger.level = 4

desc = %q
admin = User.find_by(id: -1) || User.where(admin: true).order(:id).first
raise "No admin user found. Seed the database first." if admin.nil?

# Revoke any existing keys with this description
ApiKey.where(description: desc).update_all(revoked_at: Time.current)

# Create new key
key = ApiKey.create!(
  user: admin,
  description: desc,
  created_by_id: admin.id
)

STDOUT.sync = true
puts "DV_API_KEY:#{key.key}"
puts "DV_USERNAME:#{admin.username}"
`, APIKeyDescription)

	cmd := fmt.Sprintf("cd %s && RAILS_ENV=development bundle exec rails runner - <<'RUBY'\n%s\nRUBY",
		shellQuote(c.Workdir), rubyScript)

	out, err := docker.ExecCombinedOutput(c.ContainerName, c.Workdir, c.Envs, []string{"bash", "-lc", cmd})
	c.verboseLog("Rails runner output (%d bytes, markers: key=%t, user=%t)", len(out), strings.Contains(out, "DV_API_KEY:"), strings.Contains(out, "DV_USERNAME:"))
	if err != nil {
		return fmt.Errorf("rails runner failed: %w\nOutput: %s", err, out)
	}

	// Parse output looking for DV_API_KEY: and DV_USERNAME: markers
	// This is robust against warnings/noise that plugins may emit during Rails init
	var keyLine, userLine string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "DV_API_KEY:") {
			keyLine = strings.TrimPrefix(line, "DV_API_KEY:")
		} else if strings.HasPrefix(line, "DV_USERNAME:") {
			userLine = strings.TrimPrefix(line, "DV_USERNAME:")
		}
	}

	if keyLine == "" || userLine == "" {
		return fmt.Errorf("missing DV_API_KEY or DV_USERNAME markers in output (stderr/warnings may have caused issues): %q", out)
	}

	// Validate key format (should be hex, 32-64 chars)
	keyRe := regexp.MustCompile(`^[0-9a-f]{32,64}$`)
	if !keyRe.MatchString(keyLine) {
		return fmt.Errorf("invalid API key format: %q", keyLine)
	}

	c.APIKey = keyLine
	c.APIUsername = userLine

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
