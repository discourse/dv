package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/discourse"
	"dv/internal/docker"
	"dv/internal/xdg"
)

var aiFeatureSettings = []string{
	"ai_sentiment_enabled",
	"ai_helper_enabled",
	"ai_embeddings_enabled",
	"ai_embeddings_per_post_enabled",
	"ai_embeddings_semantic_related_topics_enabled",
	"ai_embeddings_semantic_search_enabled",
	"ai_embeddings_semantic_quick_search_enabled",
	"ai_summarization_enabled",
	"ai_summary_gists_enabled",
	"ai_bot_enabled",
	"ai_discover_enabled",
	"ai_discord_search_enabled",
	"ai_spam_detection_enabled",
	"ai_rag_images_enabled",
	"ai_translation_enabled",
}

var configAICmd = &cobra.Command{
	Use:   "ai",
	Short: "Configure Discourse AI LLMs via a delightful TUI",
	Long: `Configure Discourse AI LLMs via a delightful TUI.

This command launches an interactive interface to configure AI language models
for your Discourse instance. You can add, edit, test, and manage LLM providers
including OpenAI, Anthropic, OpenRouter, and more.

ENVIRONMENT VARIABLES

The following environment variables are automatically used if set:

OpenAI:
  OPENAI_API_KEY          OpenAI API key for GPT models

Anthropic:
  ANTHROPIC_API_KEY       Anthropic API key for Claude models

OpenRouter:
  OPENROUTER_API_KEY      OpenRouter API key
  OPENROUTER_KEY          Alternative OpenRouter API key variable

Other Providers:
  GROQ_API_KEY            Groq API key for fast inference models
  GEMINI_API_KEY          Google Gemini API key
  GOOGLE_API_KEY          Alternate Google Gemini API key variable
  DEEPSEEK_API_KEY        DeepSeek API key

GitHub:
  GH_TOKEN                GitHub access token for AI bot GitHub integration

AWS Bedrock:
  AWS_ACCESS_KEY_ID       AWS access key for Bedrock
  AWS_SECRET_ACCESS_KEY   AWS secret key for Bedrock

These environment variables are automatically populated in API key fields when
configuring new models, and are passed to the container when testing connections.
`,
	RunE: func(cmd *cobra.Command, args []string) error {
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
			fmt.Fprintln(cmd.ErrOrStderr(), "No container selected. Run 'dv start' or pass --container.")
			return nil
		}

		if !docker.Exists(containerName) {
			fmt.Fprintf(cmd.OutOrStdout(), "Container '%s' does not exist. Run 'dv start' first.\n", containerName)
			return nil
		}
		if !docker.Running(containerName) {
			fmt.Fprintf(cmd.OutOrStdout(), "Starting container '%s'...\n", containerName)
			if err := docker.Start(containerName); err != nil {
				return err
			}
		}

		imgName := cfg.ContainerImages[containerName]
		var imgCfg config.ImageConfig
		if imgName != "" {
			imgCfg = cfg.Images[imgName]
		} else {
			_, resolved, err := resolveImage(cfg, "")
			if err != nil {
				return err
			}
			imgCfg = resolved
		}
		discourseRoot := strings.TrimSpace(imgCfg.Workdir)
		if discourseRoot == "" {
			discourseRoot = "/var/www/discourse"
		}

		verbose, _ := cmd.Flags().GetBool("verbose")
		client, err := discourse.NewClientWrapper(containerName, cfg, collectEnvPassthrough(cfg), verbose)
		if err != nil {
			return fmt.Errorf("create discourse client: %w", err)
		}

		cacheDir, err := xdg.CacheDir()
		if err != nil {
			return err
		}
		providerCache := filepath.Join(cacheDir, "ai_models")
		env := map[string]string{}
		for _, kv := range os.Environ() {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) == 2 {
				env[parts[0]] = parts[1]
			}
		}

		model := newAiConfigModel(aiConfigOptions{
			client:       client,
			env:          env,
			container:    containerName,
			discourseDir: discourseRoot,
			ctx:          cmd.Context(),
			loadingState: true,
			cacheDir:     providerCache,
		})

		program := tea.NewProgram(model, tea.WithContext(cmd.Context()))
		if _, runErr := program.Run(); runErr != nil {
			return runErr
		}
		return nil
	},
}

func init() {
	configAICmd.Flags().String("container", "", "Container to configure (defaults to selected agent)")
	configAICmd.Flags().Bool("verbose", false, "Print verbose debugging output")
	configCmd.AddCommand(configAICmd)
}

func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	return s[:limit] + "...(truncated)"
}
