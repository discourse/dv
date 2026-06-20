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

type aiConfigRuntime struct {
	containerName string
	discourseRoot string
	client        *discourse.ClientWrapper
}

var configAICmd = &cobra.Command{
	Use:   "ai [config.json|alias]",
	Short: "Configure Discourse AI LLMs via a delightful TUI or JSON file",
	Long: `Configure Discourse AI LLMs via a delightful TUI or JSON file.

With no arguments this command launches an interactive interface to configure AI
language models for your Discourse instance. You can add, edit, test, and manage
LLM providers including OpenAI, Anthropic, OpenRouter, and more.

Pass a JSON file (or an alias under ~/.config/dv/ai/) to apply LLM configuration
non-interactively, for example:

  dv config ai ./cdck_qwen.json
  dv config ai cdck_qwen

ENVIRONMENT VARIABLES

The following environment variables are automatically used if set:

OpenAI:
  OPENAI_API_KEY          OpenAI API key for GPT models

Anthropic:
  ANTHROPIC_API_KEY       Anthropic API key for Claude models

OpenRouter:
  OPENROUTER_API_KEY      OpenRouter API key
  OPENROUTER_KEY          Alternative OpenRouter API key variable

Venice AI:
  VENICE_API_KEY          Venice AI API key

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

JSON files may also refer to secrets via api_key_ref (op://...), api_key_env, or
api_key. Secret values are never printed.
`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeAIConfigArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return runConfigAIFile(cmd, args[0])
		}
		return runConfigAITUI(cmd)
	},
}

func runConfigAITUI(cmd *cobra.Command) error {
	runtime, err := setupAIConfigRuntime(cmd)
	if err != nil {
		return err
	}
	if runtime.client == nil {
		return nil
	}

	cacheDir, err := xdg.CacheDir()
	if err != nil {
		return err
	}
	providerCache := filepath.Join(cacheDir, "ai_models")

	model := newAiConfigModel(aiConfigOptions{
		client:       runtime.client,
		env:          currentEnvironmentMap(),
		container:    runtime.containerName,
		discourseDir: runtime.discourseRoot,
		ctx:          cmd.Context(),
		loadingState: true,
		cacheDir:     providerCache,
	})

	program := tea.NewProgram(model, tea.WithContext(cmd.Context()))
	if _, runErr := program.Run(); runErr != nil {
		return runErr
	}
	return nil
}

func setupAIConfigRuntime(cmd *cobra.Command) (aiConfigRuntime, error) {
	var runtime aiConfigRuntime

	configDir, err := xdg.ConfigDir()
	if err != nil {
		return runtime, err
	}

	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		return runtime, err
	}

	containerOverride, _ := cmd.Flags().GetString("container")
	containerName := strings.TrimSpace(containerOverride)
	if containerName == "" {
		containerName = currentAgentName(cfg)
	}
	if containerName == "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "No container selected. Run 'dv start' or pass --container.")
		return runtime, nil
	}
	runtime.containerName = containerName

	if !docker.Exists(containerName) {
		fmt.Fprintf(cmd.OutOrStdout(), "Container '%s' does not exist. Run 'dv start' first.\n", containerName)
		return runtime, nil
	}
	if !docker.Running(containerName) {
		fmt.Fprintf(cmd.OutOrStdout(), "Starting container '%s'...\n", containerName)
		if err := startContainerWithPostStartHook(cmd, cfg, configDir, containerName, "config ai"); err != nil {
			return runtime, err
		}
	}

	imgName := cfg.ContainerImages[containerName]
	var imgCfg config.ImageConfig
	if imgName != "" {
		imgCfg = cfg.Images[imgName]
	} else {
		_, resolved, err := resolveImage(cfg, "")
		if err != nil {
			return runtime, err
		}
		imgCfg = resolved
	}
	discourseRoot := strings.TrimSpace(imgCfg.Workdir)
	if discourseRoot == "" {
		discourseRoot = "/var/www/discourse"
	}
	runtime.discourseRoot = discourseRoot

	verbose, _ := cmd.Flags().GetBool("verbose")
	client, err := discourse.NewClientWrapper(containerName, cfg, collectEnvPassthrough(cfg), verbose)
	if err != nil {
		return runtime, fmt.Errorf("create discourse client: %w", err)
	}
	runtime.client = client

	return runtime, nil
}

func currentEnvironmentMap() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}
	return env
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
