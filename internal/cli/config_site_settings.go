package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"dv/internal/config"
	"dv/internal/discourse"
	"dv/internal/docker"
	"dv/internal/onepassword"
	"dv/internal/xdg"
)

var configSiteSettingsCmd = &cobra.Command{
	Use:   "site_settings FILENAME",
	Short: "Apply site settings from a YAML file",
	Long: `Apply Discourse site settings from a YAML file.

The YAML file should contain key-value pairs where keys are setting names
and values are the desired values:

  title: "My Forum"
  site_description: "A community forum"
  ai_bot_enabled: true
  max_image_size_kb: 4096

1PASSWORD INTEGRATION

Values can reference 1Password secrets using the op:// syntax:

  openai_api_key: op://Development/OpenAI/api-key
  anthropic_api_key: op://Development/Anthropic/credential

The 1Password CLI (op) must be installed and authenticated for this to work.

FLAGS

  --dry-run    Preview changes without applying them
  --container  Target a specific container (defaults to selected agent)
`,
	RunE: runSiteSettings,
}

func init() {
	configSiteSettingsCmd.Flags().Bool("dry-run", false, "Preview changes without applying")
	configSiteSettingsCmd.Flags().String("container", "", "Target container (defaults to selected agent)")
	configCmd.AddCommand(configSiteSettingsCmd)
}

type settingResult struct {
	name   string
	value  interface{}
	fromOP bool
	opRef  string
	status string // "changed", "unchanged", "error"
	err    error
}

func runSiteSettings(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Missing required argument: FILENAME")
		return cmd.Help()
	}
	filename := args[0]
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	// Load config and resolve container
	configDir, err := xdg.ConfigDir()
	if err != nil {
		return err
	}
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		return err
	}

	containerOverride, _ := cmd.Flags().GetString("container")
	containerName := strings.TrimSpace(containerOverride)
	if containerName == "" {
		containerName = currentAgentName(cfg)
	}
	if containerName == "" {
		return fmt.Errorf("no container selected; run 'dv start' or pass --container")
	}

	// Read and parse YAML file
	data, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("read settings file: %w", err)
	}

	var settings map[string]interface{}
	if err := yaml.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("parse YAML: %w", err)
	}

	return ApplySiteSettings(cmd, cfg, containerName, settings, collectEnvPassthrough(cfg), dryRun, filename)
}

func ApplySiteSettings(cmd *cobra.Command, cfg config.Config, containerName string, settings map[string]interface{}, envs docker.Envs, dryRun bool, filename string) error {
	// Check container state
	if !docker.Exists(containerName) {
		return fmt.Errorf("container '%s' does not exist; run 'dv start' first", containerName)
	}
	if !docker.Running(containerName) {
		fmt.Fprintf(cmd.OutOrStdout(), "Starting container '%s'...\n", containerName)
		configDir, _ := xdg.ConfigDir()
		if err := startContainerWithPostStartHook(cmd, cfg, configDir, containerName, "config site_settings"); err != nil {
			return err
		}
	}

	if len(settings) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No settings found in file.")
		return nil
	}

	// Track original op:// references for display
	opRefs := make(map[string]string)
	for key, value := range settings {
		if str, ok := value.(string); ok && onepassword.IsReference(str) {
			opRefs[key] = str
		}
	}

	// Check for 1Password references
	hasOPRefs := len(opRefs) > 0
	if hasOPRefs && !onepassword.CLIAvailable() {
		fmt.Fprintln(cmd.ErrOrStderr(), "Warning: 1Password CLI (op) not found. op:// references will fail.")
		fmt.Fprintln(cmd.ErrOrStderr(), "Install from: https://developer.1password.com/docs/cli/get-started/")
	}

	// Resolve 1Password references
	resolved, fromOP, resolveErr := onepassword.ResolveSettings(settings)
	if resolveErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v\n", resolveErr)
	}

	// Sort keys for consistent output
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	if dryRun {
		fmt.Fprintln(cmd.OutOrStdout(), "DRY RUN - No changes will be made")
		fmt.Fprintln(cmd.OutOrStdout(), "Would apply:")
		for _, key := range keys {
			value, ok := resolved[key]
			if !ok {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s: <failed to resolve>\n", key)
				continue
			}
			if fromOP[key] {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s (from %s)\n", key, maskValue(value), opRefs[key])
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s\n", key, formatValue(value))
			}
		}
		return nil
	}

	// Create Discourse client
	client, err := discourse.NewClientWrapper(containerName, cfg, envs, false)
	if err != nil {
		return fmt.Errorf("create discourse client: %w", err)
	}
	if err := client.EnsureAPIKey(); err != nil {
		return fmt.Errorf("ensure API key: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Applying site settings from %s...\n\n", filename)

	// Apply settings and collect results
	var results []settingResult
	for _, key := range keys {
		value, ok := resolved[key]
		if !ok {
			results = append(results, settingResult{
				name:   key,
				fromOP: fromOP[key],
				opRef:  opRefs[key],
				status: "error",
				err:    fmt.Errorf("failed to resolve value"),
			})
			continue
		}

		// Get current value to check if it's unchanged
		currentValue, getErr := client.GetSiteSetting(key)

		result := settingResult{
			name:   key,
			value:  value,
			fromOP: fromOP[key],
			opRef:  opRefs[key],
		}

		// Check if unchanged (comparing string representations for simplicity)
		if getErr == nil && fmt.Sprintf("%v", currentValue) == fmt.Sprintf("%v", value) {
			result.status = "unchanged"
		} else {
			// Apply the setting
			if err := client.SetSiteSetting(key, value); err != nil {
				result.status = "error"
				result.err = err
			} else {
				result.status = "changed"
			}
		}

		results = append(results, result)
	}

	// Print results
	var changed, unchanged, errored int
	for _, r := range results {
		var statusStr string
		switch r.status {
		case "changed":
			statusStr = "[changed]"
			changed++
		case "unchanged":
			statusStr = "[unchanged]"
			unchanged++
		case "error":
			statusStr = fmt.Sprintf("[error: %v]", r.err)
			errored++
		}

		if r.fromOP {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s (from 1Password) %s\n", r.name, maskValue(r.value), statusStr)
		} else if r.status == "error" && r.value == nil {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s\n", r.name, statusStr)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s %s\n", r.name, formatValue(r.value), statusStr)
		}
	}

	// Summary
	fmt.Fprintf(cmd.OutOrStdout(), "\nApplied %d settings", changed)
	if unchanged > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), " (%d unchanged)", unchanged)
	}
	if errored > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), " (%d errors)", errored)
	}
	fmt.Fprintln(cmd.OutOrStdout())

	return nil
}

// formatValue formats a value for display
func formatValue(v interface{}) string {
	switch val := v.(type) {
	case string:
		if len(val) > 60 {
			return fmt.Sprintf("%q", val[:57]+"...")
		}
		return fmt.Sprintf("%q", val)
	case bool:
		return fmt.Sprintf("%v", val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

// maskValue masks a secret value for display
func maskValue(v interface{}) string {
	str := fmt.Sprintf("%v", v)
	if len(str) <= 8 {
		return "********"
	}
	return str[:4] + "********"
}
