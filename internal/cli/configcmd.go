package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/xdg"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage dv configuration",
}

var configGetCmd = &cobra.Command{
	Use:   "get KEY",
	Short: "Get a config value",
	Args:  cobra.ExactArgs(1),
	ValidArgs: []string{
		"imageTag", "defaultContainerName", "workdir", "customWorkdir",
		"hostStartingPort", "containerPort", "selectedAgent", "discourseRepo",
		"extractBranchPrefix", "defaultTemplate",
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}
		key := args[0]
		val, err := getConfigField(cfg, key)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), val)
		return nil
	},
}

var configSetCmd = &cobra.Command{
	Use:   "set KEY VALUE",
	Short: "Set a config value",
	Args:  cobra.ExactArgs(2),
	ValidArgs: []string{
		"imageTag", "defaultContainerName", "workdir", "customWorkdir",
		"hostStartingPort", "containerPort", "selectedAgent", "discourseRepo",
		"extractBranchPrefix", "defaultTemplate",
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}
		key, value := args[0], args[1]
		if err := setConfigField(&cfg, key, value); err != nil {
			return err
		}
		return config.Save(configDir, cfg)
	},
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show full config JSON",
	RunE: func(cmd *cobra.Command, args []string) error {
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return nil
	},
}

var configEditCmd = &cobra.Command{
	Use:   "edit",
	Short: "Edit config file in your editor",
	RunE: func(cmd *cobra.Command, args []string) error {
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		// Ensure config exists
		_, err = config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}

		configPath := config.Path(configDir)
		editor := getEditor()

		editorCmd := exec.Command(editor, configPath)
		editorCmd.Stdin = os.Stdin
		editorCmd.Stdout = os.Stdout
		editorCmd.Stderr = os.Stderr

		return editorCmd.Run()
	},
}

var configResetCmd = &cobra.Command{
	Use:   "reset [KEY]",
	Short: "Reset config (or a specific key) to default values",
	Args:  cobra.MaximumNArgs(1),
	ValidArgs: []string{
		"copyRules",
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}

		if len(args) == 0 {
			cfg := config.Default()
			if err := config.Save(configDir, cfg); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Config reset to default values")
			return nil
		}

		key := args[0]
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}

		switch key {
		case "copyRules":
			cfg.CopyRules = config.DefaultCopyRules()
			fmt.Fprintln(cmd.OutOrStdout(), "Config key 'copyRules' reset to default values")
		default:
			// Fallback to checking if we can just set it to default from a new Default config
			return fmt.Errorf("resetting key %q is not supported yet (try 'dv config reset' for everything)", key)
		}

		return config.Save(configDir, cfg)
	},
}

func init() {
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configShowCmd)
	configCmd.AddCommand(configEditCmd)
	configCmd.AddCommand(configResetCmd)
}

func getConfigField(cfg config.Config, key string) (string, error) {
	switch key {
	case "imageTag":
		return cfg.ImageTag, nil
	case "defaultContainerName":
		return cfg.DefaultContainer, nil
	case "workdir":
		return cfg.Workdir, nil
	case "customWorkdir":
		return cfg.CustomWorkdir, nil
	case "hostStartingPort":
		return fmt.Sprint(cfg.HostStartingPort), nil
	case "containerPort":
		return fmt.Sprint(cfg.ContainerPort), nil
	case "selectedAgent":
		return cfg.SelectedAgent, nil
	case "discourseRepo":
		return cfg.DiscourseRepo, nil
	case "extractBranchPrefix":
		return cfg.ExtractBranchPrefix, nil
	case "defaultTemplate":
		return cfg.DefaultTemplate, nil
	default:
		return "", fmt.Errorf("unknown key: %s", key)
	}
}

func setConfigField(cfg *config.Config, key, val string) error {
	switch key {
	case "imageTag":
		cfg.ImageTag = val
	case "defaultContainerName":
		cfg.DefaultContainer = val
	case "workdir":
		cfg.Workdir = val
	case "customWorkdir":
		cfg.CustomWorkdir = val
	case "hostStartingPort":
		var v int
		_, err := fmt.Sscanf(val, "%d", &v)
		if err != nil {
			return err
		}
		cfg.HostStartingPort = v
	case "containerPort":
		var v int
		_, err := fmt.Sscanf(val, "%d", &v)
		if err != nil {
			return err
		}
		cfg.ContainerPort = v
	case "selectedAgent":
		cfg.SelectedAgent = val
	case "discourseRepo":
		cfg.DiscourseRepo = val
	case "extractBranchPrefix":
		cfg.ExtractBranchPrefix = val
	case "defaultTemplate":
		cfg.DefaultTemplate = val
	default:
		return fmt.Errorf("unknown key: %s", key)
	}
	return nil
}

// getEditor returns the user's preferred editor based on environment variables
// or a sensible default for the platform.
func getEditor() string {
	// Check VISUAL first (for full-screen editors)
	if editor := os.Getenv("VISUAL"); editor != "" {
		return editor
	}
	// Fall back to EDITOR
	if editor := os.Getenv("EDITOR"); editor != "" {
		return editor
	}
	// Default to vi (available on virtually all Unix systems)
	return "vi"
}
