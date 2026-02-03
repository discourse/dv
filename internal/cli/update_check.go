package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"dv/internal/xdg"
)

const (
	updateStateFilename   = "update-state.json"
	updateCheckInterval   = 24 * time.Hour
	updateUserAgent       = "dv-cli"
	skipUpdateEnvVar      = "DV_SKIP_UPDATE_CHECK"
	updateRepoOwner       = "discourse"
	updateRepoName        = "dv"
	updateCheckCommandArg = "__update-check"
)

var updateCheckCmd = &cobra.Command{
	Use:    updateCheckCommandArg,
	Short:  "internal update check",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if os.Getenv(skipUpdateEnvVar) == "" {
			// Ensure the background process does not try to spawn itself again.
			os.Setenv(skipUpdateEnvVar, "1")
		}
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		return performUpdateCheck(cmd.Context(), configDir)
	},
}

type updateState struct {
	LastChecked     time.Time `json:"lastChecked"`
	LatestVersion   string    `json:"latestVersion"`
	LastError       string    `json:"lastError,omitempty"`
	NotifiedVersion string    `json:"notifiedVersion,omitempty"`
	LastNotified    time.Time `json:"lastNotified,omitempty"`
}

type releaseInfo struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func setupUpdateChecks() {
	rootCmd.AddCommand(updateCheckCmd)
	cobra.OnInitialize(func() {
		if os.Getenv(skipUpdateEnvVar) != "" {
			return
		}
		if version == "dev" {
			return
		}
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return
		}
		state, err := loadUpdateState(configDir)
		if err == nil {
			warnIfOutdated(configDir, state)
		}
		if shouldCheckForUpdates(state) {
			if err := launchBackgroundUpdateCheck(); err != nil {
				fmt.Fprintf(os.Stderr, "dv: failed to schedule update check: %v\n", err)
			}
		}
	})
}

func shouldCheckForUpdates(state *updateState) bool {
	if state == nil {
		return true
	}
	if state.LastChecked.IsZero() {
		return true
	}
	return time.Since(state.LastChecked) >= updateCheckInterval
}

func launchBackgroundUpdateCheck() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, updateCheckCommandArg)
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=1", skipUpdateEnvVar))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Start()
}

func performUpdateCheck(ctx context.Context, configDir string) error {
	state, err := loadUpdateState(configDir)
	if err != nil {
		state = &updateState{}
	}
	info, err := fetchLatestRelease(ctx)
	now := time.Now().UTC()
	if err != nil {
		state.LastChecked = now
		state.LastError = err.Error()
		return saveUpdateState(configDir, state)
	}
	state.LastChecked = now
	state.LatestVersion = info.TagName
	state.LastError = ""
	// Preserve notified fields; they'll be updated when we warn next time the CLI runs.
	return saveUpdateState(configDir, state)
}

func warnIfOutdated(configDir string, state *updateState) {
	if state == nil {
		return
	}
	latest := strings.TrimSpace(state.LatestVersion)
	if latest == "" {
		return
	}
	if !isVersionOutdated(version, latest) {
		return
	}
	shouldWarn := state.NotifiedVersion != latest || state.LastNotified.IsZero() || time.Since(state.LastNotified) >= updateCheckInterval
	if !shouldWarn {
		return
	}
	fmt.Fprintf(os.Stderr, "A newer dv release (%s) is available. Run 'dv upgrade' to update.\n", latest)
	state.NotifiedVersion = latest
	state.LastNotified = time.Now().UTC()
	_ = saveUpdateState(configDir, state)
}

func loadUpdateState(configDir string) (*updateState, error) {
	path := filepath.Join(configDir, updateStateFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &updateState{}, nil
		}
		return nil, err
	}
	var state updateState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func saveUpdateState(configDir string, state *updateState) error {
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(configDir, "update-state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	path := filepath.Join(configDir, updateStateFilename)
	return os.Rename(tmp.Name(), path)
}

func fetchLatestRelease(ctx context.Context) (*releaseInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", updateRepoOwner, updateRepoName), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", updateUserAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("unexpected response %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var info releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	if strings.TrimSpace(info.TagName) == "" {
		return nil, errors.New("latest release is missing tag name")
	}
	return &info, nil
}

func isVersionOutdated(current, latest string) bool {
	current = normalizeVersion(current)
	latest = normalizeVersion(latest)
	if current == "" || latest == "" {
		return false
	}
	cmp, ok := compareVersionStrings(current, latest)
	if !ok {
		return false
	}
	return cmp < 0
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexFunc(v, func(r rune) bool { return !(r >= '0' && r <= '9') && r != '.' }); i >= 0 {
		v = v[:i]
	}
	return v
}

func compareVersionStrings(a, b string) (int, bool) {
	aParts, ok := parseVersionParts(a)
	if !ok {
		return 0, false
	}
	bParts, ok := parseVersionParts(b)
	if !ok {
		return 0, false
	}
	maxLen := len(aParts)
	if len(bParts) > maxLen {
		maxLen = len(bParts)
	}
	for len(aParts) < maxLen {
		aParts = append(aParts, 0)
	}
	for len(bParts) < maxLen {
		bParts = append(bParts, 0)
	}
	for i := 0; i < maxLen; i++ {
		if aParts[i] < bParts[i] {
			return -1, true
		}
		if aParts[i] > bParts[i] {
			return 1, true
		}
	}
	return 0, true
}

func parseVersionParts(v string) ([]int, bool) {
	if v == "" {
		return nil, false
	}
	pieces := strings.Split(v, ".")
	parts := make([]int, len(pieces))
	for i, piece := range pieces {
		if piece == "" {
			return nil, false
		}
		n, err := strconv.Atoi(piece)
		if err != nil {
			return nil, false
		}
		parts[i] = n
	}
	return parts, true
}
