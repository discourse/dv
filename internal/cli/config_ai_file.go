package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/ai"
	"dv/internal/discourse"
	"dv/internal/onepassword"
	"dv/internal/xdg"
)

const (
	defaultAIFileVersion         = 1
	defaultAIFileProvider        = "vllm"
	defaultAIFileTokenizer       = "DiscourseAi::Tokenizer::OpenAiTokenizer"
	defaultAIFileMaxPromptTokens = 200000
	defaultAIFileMaxOutputTokens = 50000
)

type aiFileConfig struct {
	Version      int                    `json:"version"`
	LLMs         []aiFileLLM            `json:"llms"`
	SiteSettings map[string]interface{} `json:"site_settings"`
}

type aiFileLLM struct {
	ID              string                 `json:"id"`
	DisplayName     string                 `json:"display_name"`
	Name            string                 `json:"name"`
	Provider        string                 `json:"provider"`
	Tokenizer       string                 `json:"tokenizer"`
	URL             string                 `json:"url"`
	BaseURL         string                 `json:"base_url"`
	APIKey          string                 `json:"api_key"`
	APIKeyRef       string                 `json:"api_key_ref"`
	APIKeyEnv       string                 `json:"api_key_env"`
	AiSecretID      int64                  `json:"ai_secret_id"`
	AiSecretName    string                 `json:"ai_secret_name"`
	MaxPromptTokens int                    `json:"max_prompt_tokens"`
	MaxOutputTokens int                    `json:"max_output_tokens"`
	InputCost       float64                `json:"input_cost"`
	CachedInputCost float64                `json:"cached_input_cost"`
	OutputCost      float64                `json:"output_cost"`
	EnabledChatBot  bool                   `json:"enabled_chat_bot"`
	VisionEnabled   bool                   `json:"vision_enabled"`
	SetAsDefault    bool                   `json:"set_as_default"`
	Test            bool                   `json:"test"`
	ProviderParams  map[string]interface{} `json:"provider_params"`
	Upsert          aiFileUpsert           `json:"upsert"`
}

type aiFileUpsert struct {
	Match string `json:"match"`
}

type aiFileDiscourseClient interface {
	discourse.DiscourseClient
	SetSiteSetting(name string, value interface{}) error
}

func completeAIConfigArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	// If the user is typing an explicit path, let the shell complete JSON files.
	if aiConfigCompletionLooksLikePath(toComplete) {
		return []string{"json"}, cobra.ShellCompDirectiveFilterFileExt
	}

	configDir, err := xdg.ConfigDir()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	suggestions := completeAIConfigAliases(filepath.Join(configDir, "ai"), toComplete)
	return suggestions, cobra.ShellCompDirectiveNoFileComp
}

func aiConfigCompletionLooksLikePath(toComplete string) bool {
	return strings.HasPrefix(toComplete, "./") || strings.HasPrefix(toComplete, "../") || strings.HasPrefix(toComplete, "/") || strings.HasPrefix(toComplete, "~")
}

func completeAIConfigAliases(dir, toComplete string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	prefix := strings.ToLower(strings.TrimSpace(toComplete))
	var suggestions []string
	seen := map[string]struct{}{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}

		alias := strings.TrimSuffix(name, filepath.Ext(name))
		for _, candidate := range []string{alias, name} {
			if candidate == "" {
				continue
			}
			if prefix != "" && !strings.HasPrefix(strings.ToLower(candidate), prefix) {
				continue
			}
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}
			suggestions = append(suggestions, fmt.Sprintf("%s\t%s", candidate, filepath.Join(dir, name)))
		}
	}
	sort.Strings(suggestions)
	return suggestions
}

func runConfigAIFile(cmd *cobra.Command, arg string) error {
	configDir, err := xdg.ConfigDir()
	if err != nil {
		return err
	}

	path, err := resolveAIConfigFilePath(configDir, arg)
	if err != nil {
		return err
	}

	fileCfg, err := readAIFileConfig(path)
	if err != nil {
		return err
	}

	runtime, err := setupAIConfigRuntime(cmd)
	if err != nil {
		return err
	}
	if runtime.client == nil {
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Applying AI config %s to container %s...\n", path, runtime.containerName)
	return applyAIFileConfig(cmd.Context(), cmd.OutOrStdout(), runtime.client, fileCfg)
}

func resolveAIConfigFilePath(configDir, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", errors.New("AI config file path or alias is required")
	}

	isExplicitPath := filepath.IsAbs(arg) || strings.ContainsAny(arg, `/\\`) || strings.HasPrefix(arg, ".") || strings.HasPrefix(arg, "~") || strings.Contains(arg, "$")
	var searched []string

	if isExplicitPath {
		path := expandHostPath(arg)
		if fileExists(path) {
			return path, nil
		}
		return "", fmt.Errorf("AI config file not found: %s", path)
	}

	localPath := expandHostPath(arg)
	searched = append(searched, localPath)
	if fileExists(localPath) {
		return localPath, nil
	}

	aliases := []string{filepath.Join(configDir, "ai", arg)}
	if filepath.Ext(arg) == "" {
		aliases = append(aliases, filepath.Join(configDir, "ai", arg+".json"))
	}
	for _, candidate := range aliases {
		searched = append(searched, candidate)
		if fileExists(candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("AI config %q not found (searched: %s)", arg, strings.Join(searched, ", "))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func readAIFileConfig(path string) (aiFileConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return aiFileConfig{}, fmt.Errorf("open AI config %s: %w", path, err)
	}
	defer f.Close()

	cfg, err := decodeAIFileConfig(f)
	if err != nil {
		return aiFileConfig{}, fmt.Errorf("decode AI config %s: %w", path, err)
	}
	return cfg, nil
}

func decodeAIFileConfig(r io.Reader) (aiFileConfig, error) {
	var cfg aiFileConfig
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return cfg, errors.New("unexpected extra JSON data")
		}
		return cfg, err
	}
	if err := validateAIFileConfig(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func validateAIFileConfig(cfg aiFileConfig) error {
	if cfg.Version == 0 {
		cfg.Version = defaultAIFileVersion
	}
	if cfg.Version != defaultAIFileVersion {
		return fmt.Errorf("unsupported AI config version %d", cfg.Version)
	}
	if len(cfg.LLMs) == 0 && len(cfg.SiteSettings) == 0 {
		return errors.New("AI config must contain at least one llm or site_settings entry")
	}

	seenIDs := map[string]struct{}{}
	type seenLLMMatch struct {
		index int
		mode  string
	}
	seenNameProvider := map[string]seenLLMMatch{}
	seenName := map[string]seenLLMMatch{}
	for i, llm := range cfg.LLMs {
		if id := strings.TrimSpace(llm.ID); id != "" {
			key := strings.ToLower(id)
			if _, ok := seenIDs[key]; ok {
				return fmt.Errorf("llms[%d]: duplicate id %q", i, id)
			}
			seenIDs[key] = struct{}{}
		}

		input, err := buildAIFileLLMInput(llm)
		if err != nil {
			return fmt.Errorf("llms[%d] (%s): %w", i, aiFileLLMLabel(llm), err)
		}
		mode, err := normalizeAIFileUpsertMatch(llm.Upsert.Match)
		if err != nil {
			return fmt.Errorf("llms[%d] (%s): %w", i, aiFileLLMLabel(llm), err)
		}

		nameProviderKey := strings.ToLower(strings.TrimSpace(input.Name)) + "\x00" + strings.ToLower(strings.TrimSpace(input.Provider))
		if first, ok := seenNameProvider[nameProviderKey]; ok && (mode == "name_provider" || first.mode == "name_provider") {
			return fmt.Errorf("llms[%d] (%s): duplicates name/provider with llms[%d]; use unique name/provider values or set upsert.match to \"none\" for intentional duplicate creation", i, aiFileLLMLabel(llm), first.index)
		}
		if _, ok := seenNameProvider[nameProviderKey]; !ok {
			seenNameProvider[nameProviderKey] = seenLLMMatch{index: i, mode: mode}
		}

		nameKey := strings.ToLower(strings.TrimSpace(input.Name))
		if first, ok := seenName[nameKey]; ok && (mode == "name" || first.mode == "name") {
			return fmt.Errorf("llms[%d] (%s): duplicates name with llms[%d]; use unique name values or set upsert.match to \"none\" for intentional duplicate creation", i, aiFileLLMLabel(llm), first.index)
		}
		if _, ok := seenName[nameKey]; !ok {
			seenName[nameKey] = seenLLMMatch{index: i, mode: mode}
		}
	}
	return nil
}

func applyAIFileConfig(ctx context.Context, out io.Writer, client aiFileDiscourseClient, cfg aiFileConfig) error {
	modelIDs := map[string]int64{}
	apiKeys := make([]string, len(cfg.LLMs))
	apiKeySources := make([]string, len(cfg.LLMs))
	for i, llm := range cfg.LLMs {
		apiKey, source, err := resolveAIFileAPIKey(llm, os.Getenv, onepassword.Read)
		if err != nil {
			return fmt.Errorf("%s: %w", aiFileLLMLabel(llm), err)
		}
		apiKeys[i] = apiKey
		apiKeySources[i] = source
	}

	redactionSecrets := aiFileRedactionSecrets(cfg, apiKeys)

	if err := applyAIFileSiteSettings(out, client, cfg.SiteSettings, modelIDs, false); err != nil {
		return redactAIFileSecrets(err, redactionSecrets...)
	}

	var state ai.LLMState
	var err error
	if len(cfg.LLMs) > 0 || aiFileHasModelRefSettings(cfg.SiteSettings) {
		state, err = client.FetchState(ctx)
		if err != nil {
			return fmt.Errorf("fetch Discourse AI state: %w", err)
		}
	}

	for i, llm := range cfg.LLMs {
		label := aiFileLLMLabel(llm)
		input, err := buildAIFileLLMInput(llm)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}

		existing, err := findAIFileExistingLLM(state.Models, llm.Upsert.Match, input)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if existing != nil {
			input.ExistingID = existing.ID
			input.ExistingAiSecretID = existing.AiSecretID
		}

		apiKey := apiKeys[i]
		source := apiKeySources[i]

		testInput, err := buildAIFileTestInput(state, llm, input, apiKey)
		if err != nil {
			return fmt.Errorf("%s: %w", label, redactAIFileSecrets(err, apiKey, llm.APIKey))
		}

		if existing == nil && testInput.Provider != "aws_bedrock" && strings.TrimSpace(testInput.APIKey) == "" && testInput.AiSecretID == 0 {
			return fmt.Errorf("%s: API key is required for new %s LLM (set api_key_ref, api_key_env, api_key, ai_secret_id, or ai_secret_name)", label, input.Provider)
		}

		if llm.Test {
			fmt.Fprintf(out, "Testing LLM %s...\n", label)
			if err := client.TestModel(ctx, testInput); err != nil {
				return fmt.Errorf("%s: test failed: %w", label, redactAIFileSecrets(err, apiKey, llm.APIKey))
			}
			fmt.Fprintf(out, "Test passed for %s.\n", label)
		}

		input, err = prepareAIFileCredentials(ctx, client, state, llm, input, apiKey)
		if err != nil {
			return fmt.Errorf("%s: %w", label, redactAIFileSecrets(err, apiKey, llm.APIKey))
		}

		if existing != nil {
			if source != "" {
				fmt.Fprintf(out, "Updating LLM %s (id %d, secret via %s)...\n", label, existing.ID, source)
			} else {
				fmt.Fprintf(out, "Updating LLM %s (id %d)...\n", label, existing.ID)
			}
			if err := client.UpdateModel(ctx, existing.ID, input); err != nil {
				return fmt.Errorf("%s: update LLM: %w", label, redactAIFileSecrets(err, apiKey, llm.APIKey))
			}
			recordAIFileModelID(modelIDs, llm, existing.ID)
			fmt.Fprintf(out, "Updated LLM %s (id %d).\n", label, existing.ID)
		} else {
			if source != "" {
				fmt.Fprintf(out, "Creating LLM %s (secret via %s)...\n", label, source)
			} else {
				fmt.Fprintf(out, "Creating LLM %s...\n", label)
			}
			id, err := client.CreateModel(ctx, input)
			if err != nil {
				return fmt.Errorf("%s: create LLM: %w", label, redactAIFileSecrets(err, apiKey, llm.APIKey))
			}
			recordAIFileModelID(modelIDs, llm, id)
			fmt.Fprintf(out, "Created LLM %s (id %d).\n", label, id)
		}

		if len(cfg.LLMs) > 1 || aiFileHasModelRefSettings(cfg.SiteSettings) {
			state, err = client.FetchState(ctx)
			if err != nil {
				return fmt.Errorf("fetch Discourse AI state after %s: %w", label, err)
			}
		}
	}

	if err := applyAIFileSiteSettings(out, client, cfg.SiteSettings, modelIDs, true); err != nil {
		return redactAIFileSecrets(err, redactionSecrets...)
	}

	fmt.Fprintln(out, "AI config applied.")
	return nil
}

func buildAIFileLLMInput(llm aiFileLLM) (discourse.CreateLLMInput, error) {
	provider := providerSlug(llm.Provider)
	if provider == "" {
		provider = defaultAIFileProvider
	}

	displayName := strings.TrimSpace(llm.DisplayName)
	name := strings.TrimSpace(llm.Name)
	tokenizer := strings.TrimSpace(llm.Tokenizer)
	if tokenizer == "" {
		tokenizer = defaultAIFileTokenizer
	}
	url := strings.TrimSpace(llm.URL)
	if url == "" && strings.TrimSpace(llm.BaseURL) != "" {
		url = aiFileChatCompletionsURL(llm.BaseURL)
	}

	if displayName == "" {
		return discourse.CreateLLMInput{}, errors.New("display_name is required")
	}
	if name == "" {
		return discourse.CreateLLMInput{}, errors.New("name is required")
	}
	if provider != "aws_bedrock" && url == "" {
		return discourse.CreateLLMInput{}, errors.New("url is required (or set base_url to append /chat/completions)")
	}
	if tokenizer == "" {
		return discourse.CreateLLMInput{}, errors.New("tokenizer is required")
	}

	maxPromptTokens := llm.MaxPromptTokens
	if maxPromptTokens <= 0 {
		maxPromptTokens = defaultAIFileMaxPromptTokens
	}
	maxOutputTokens := llm.MaxOutputTokens
	if maxOutputTokens <= 0 {
		maxOutputTokens = defaultAIFileMaxOutputTokens
	}

	providerParams := cloneStringInterfaceMap(llm.ProviderParams)

	return discourse.CreateLLMInput{
		DisplayName:     displayName,
		Name:            name,
		Provider:        provider,
		Tokenizer:       tokenizer,
		URL:             url,
		AiSecretID:      llm.AiSecretID,
		MaxPromptTokens: maxPromptTokens,
		MaxOutputTokens: maxOutputTokens,
		InputCost:       llm.InputCost,
		CachedInputCost: llm.CachedInputCost,
		OutputCost:      llm.OutputCost,
		EnabledChatBot:  llm.EnabledChatBot,
		VisionEnabled:   llm.VisionEnabled,
		ProviderParams:  providerParams,
		SetAsDefault:    llm.SetAsDefault,
	}, nil
}

func aiFileChatCompletionsURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(baseURL, "/chat/completions") {
		return baseURL
	}
	return baseURL + "/chat/completions"
}

func cloneStringInterfaceMap(in map[string]interface{}) map[string]interface{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func redactAIFileSecrets(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	redacted := msg
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret == "" {
			continue
		}
		redacted = strings.ReplaceAll(redacted, secret, "[redacted]")
	}
	if redacted == msg {
		return err
	}
	return errors.New(redacted)
}

func aiFileRedactionSecrets(cfg aiFileConfig, apiKeys []string) []string {
	var secrets []string
	secrets = append(secrets, apiKeys...)
	for _, llm := range cfg.LLMs {
		secrets = append(secrets, llm.APIKey)
	}
	for key, value := range cfg.SiteSettings {
		if aiFileSettingNameLooksSecret(key) {
			secrets = appendStringValues(secrets, value)
		}
	}
	return secrets
}

func aiFileSettingNameLooksSecret(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, marker := range []string{"key", "token", "secret", "password", "credential"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

func appendStringValues(out []string, value interface{}) []string {
	switch v := value.(type) {
	case string:
		return append(out, v)
	case []interface{}:
		for _, item := range v {
			out = appendStringValues(out, item)
		}
	case map[string]interface{}:
		for _, item := range v {
			out = appendStringValues(out, item)
		}
	}
	return out
}

func resolveAIFileAPIKey(llm aiFileLLM, getenv func(string) string, readOP func(string) (string, error)) (apiKey string, source string, err error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if readOP == nil {
		readOP = onepassword.Read
	}

	if ref := strings.TrimSpace(llm.APIKeyRef); ref != "" {
		secret, err := readOP(ref)
		if err != nil {
			return "", "api_key_ref", fmt.Errorf("resolve api_key_ref: %w", err)
		}
		secret = strings.TrimSpace(secret)
		if secret == "" {
			return "", "api_key_ref", errors.New("api_key_ref resolved to an empty value")
		}
		return secret, "api_key_ref", nil
	}

	if envName := strings.TrimSpace(llm.APIKeyEnv); envName != "" {
		secret := strings.TrimSpace(getenv(envName))
		if secret == "" {
			return "", "api_key_env", fmt.Errorf("environment variable %s is not set", envName)
		}
		return secret, "api_key_env", nil
	}

	if literal := strings.TrimSpace(llm.APIKey); literal != "" {
		return literal, "api_key", nil
	}

	return "", "", nil
}

func buildAIFileTestInput(state ai.LLMState, llm aiFileLLM, input discourse.CreateLLMInput, apiKey string) (discourse.CreateLLMInput, error) {
	testInput := input
	if strings.TrimSpace(apiKey) != "" {
		// Prefer the candidate key for tests and avoid mutating any existing
		// Discourse AiSecret until the test has passed. buildLLMPayload gives
		// AiSecretID precedence over APIKey, so clear it here.
		testInput.APIKey = apiKey
		testInput.AiSecretID = 0
		return testInput, nil
	}

	secretName := strings.TrimSpace(llm.AiSecretName)
	secretID := llm.AiSecretID
	if secretID == 0 && secretName != "" {
		secretID = findAiSecretIDByName(state.Meta.AiSecrets, secretName)
	}
	if secretID > 0 {
		testInput.AiSecretID = secretID
		testInput.APIKey = ""
		return testInput, nil
	}
	if input.ExistingAiSecretID > 0 {
		testInput.AiSecretID = input.ExistingAiSecretID
		testInput.APIKey = ""
		return testInput, nil
	}
	if secretName != "" {
		return testInput, fmt.Errorf("ai_secret_name %q was not found and no API key was supplied", secretName)
	}
	return testInput, nil
}

func prepareAIFileCredentials(ctx context.Context, client discourse.DiscourseClient, state ai.LLMState, llm aiFileLLM, input discourse.CreateLLMInput, apiKey string) (discourse.CreateLLMInput, error) {
	secretName := strings.TrimSpace(llm.AiSecretName)
	secretID := llm.AiSecretID
	if secretID == 0 && secretName != "" {
		secretID = findAiSecretIDByName(state.Meta.AiSecrets, secretName)
	}

	if strings.TrimSpace(apiKey) == "" {
		if secretID > 0 {
			input.AiSecretID = secretID
			input.APIKey = ""
			return input, nil
		}
		if input.ExistingAiSecretID > 0 {
			input.AiSecretID = input.ExistingAiSecretID
			input.APIKey = ""
			return input, nil
		}
		if secretName != "" {
			return input, fmt.Errorf("ai_secret_name %q was not found and no API key was supplied", secretName)
		}
		return input, nil
	}

	if secretID > 0 {
		if err := client.UpdateAiSecret(ctx, secretID, apiKey); err != nil {
			return input, err
		}
		input.AiSecretID = secretID
		input.APIKey = ""
		return input, nil
	}

	if secretName != "" {
		id, err := client.CreateAiSecret(ctx, secretName, apiKey)
		if err != nil {
			return input, err
		}
		input.AiSecretID = id
		input.APIKey = ""
		return input, nil
	}

	input.APIKey = apiKey
	return input, nil
}

func findAIFileExistingLLM(models []ai.LLMModel, match string, input discourse.CreateLLMInput) (*ai.LLMModel, error) {
	mode, err := normalizeAIFileUpsertMatch(match)
	if err != nil {
		return nil, err
	}
	if mode == "none" {
		return nil, nil
	}

	for i := range models {
		model := &models[i]
		switch mode {
		case "name_provider":
			if strings.EqualFold(strings.TrimSpace(model.Name), strings.TrimSpace(input.Name)) && strings.EqualFold(strings.TrimSpace(model.Provider), strings.TrimSpace(input.Provider)) {
				return model, nil
			}
		case "display_name":
			if strings.EqualFold(strings.TrimSpace(model.DisplayName), strings.TrimSpace(input.DisplayName)) {
				return model, nil
			}
		case "name":
			if strings.EqualFold(strings.TrimSpace(model.Name), strings.TrimSpace(input.Name)) {
				return model, nil
			}
		}
	}
	return nil, nil
}

func normalizeAIFileUpsertMatch(match string) (string, error) {
	match = strings.ToLower(strings.TrimSpace(match))
	if match == "" {
		return "name_provider", nil
	}
	switch match {
	case "name_provider", "display_name", "name", "none", "create":
		if match == "create" {
			return "none", nil
		}
		return match, nil
	default:
		return "", fmt.Errorf("unsupported upsert.match %q (use name_provider, display_name, name, or none)", match)
	}
}

func applyAIFileSiteSettings(out io.Writer, client aiFileDiscourseClient, settings map[string]interface{}, modelIDs map[string]int64, refsOnly bool) error {
	if len(settings) == 0 {
		return nil
	}

	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if _, ok := settings["discourse_ai_enabled"]; ok {
		reordered := []string{"discourse_ai_enabled"}
		for _, key := range keys {
			if key != "discourse_ai_enabled" {
				reordered = append(reordered, key)
			}
		}
		keys = reordered
	}

	for _, key := range keys {
		value := settings[key]
		ref, isRef := aiFileModelRef(value)
		if refsOnly != isRef {
			continue
		}
		if isRef {
			id, ok := lookupAIFileModelID(modelIDs, ref)
			if !ok {
				return fmt.Errorf("site_settings.%s references unknown LLM %q", key, ref)
			}
			value = id
		}

		fmt.Fprintf(out, "Setting site setting %s...\n", key)
		if err := client.SetSiteSetting(key, value); err != nil {
			return fmt.Errorf("set site setting %s: %w", key, err)
		}
	}
	return nil
}

func aiFileHasModelRefSettings(settings map[string]interface{}) bool {
	for _, value := range settings {
		if _, ok := aiFileModelRef(value); ok {
			return true
		}
	}
	return false
}

func aiFileModelRef(value interface{}) (string, bool) {
	s, ok := value.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	if len(s) < 2 || !strings.HasPrefix(s, "@") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(s, "@")), true
}

func recordAIFileModelID(modelIDs map[string]int64, llm aiFileLLM, id int64) {
	for _, key := range []string{llm.ID, llm.DisplayName, llm.Name} {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		modelIDs[strings.ToLower(key)] = id
	}
}

func lookupAIFileModelID(modelIDs map[string]int64, key string) (int64, bool) {
	id, ok := modelIDs[strings.ToLower(strings.TrimSpace(key))]
	return id, ok
}

func aiFileLLMLabel(llm aiFileLLM) string {
	for _, value := range []string{llm.ID, llm.DisplayName, llm.Name} {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return "unnamed"
}
