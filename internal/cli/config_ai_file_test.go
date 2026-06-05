package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"dv/internal/ai"
	"dv/internal/discourse"
)

func TestDecodeAIFileConfigRejectsUnknownFields(t *testing.T) {
	_, err := decodeAIFileConfig(strings.NewReader(`{
		"version": 1,
		"llms": [],
		"unexpected": true
	}`))
	if err == nil {
		t.Fatal("expected unknown field error")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v, want unknown field", err)
	}
}

func TestBuildAIFileLLMInputCdckQwenDefaults(t *testing.T) {
	input, err := buildAIFileLLMInput(aiFileLLM{
		ID:          "cdck_qwen",
		DisplayName: "CDCK Qwen 3.5 122B",
		Name:        "Qwen/Qwen3.5-122B-A10B",
		BaseURL:     "https://snorlax1b.dub2.discourse.cloud:23456/v1",
		ProviderParams: map[string]interface{}{
			"enable_thinking": false,
		},
	})
	if err != nil {
		t.Fatalf("buildAIFileLLMInput returned error: %v", err)
	}

	if input.Provider != "vllm" {
		t.Fatalf("Provider = %q, want vllm", input.Provider)
	}
	if input.Tokenizer != defaultAIFileTokenizer {
		t.Fatalf("Tokenizer = %q, want %q", input.Tokenizer, defaultAIFileTokenizer)
	}
	wantURL := "https://snorlax1b.dub2.discourse.cloud:23456/v1/chat/completions"
	if input.URL != wantURL {
		t.Fatalf("URL = %q, want %q", input.URL, wantURL)
	}
	if input.MaxPromptTokens != defaultAIFileMaxPromptTokens {
		t.Fatalf("MaxPromptTokens = %d, want %d", input.MaxPromptTokens, defaultAIFileMaxPromptTokens)
	}
	if input.MaxOutputTokens != defaultAIFileMaxOutputTokens {
		t.Fatalf("MaxOutputTokens = %d, want %d", input.MaxOutputTokens, defaultAIFileMaxOutputTokens)
	}
	if got, ok := input.ProviderParams["enable_thinking"].(bool); !ok || got {
		t.Fatalf("enable_thinking = %#v, want false", input.ProviderParams["enable_thinking"])
	}
	if _, ok := input.ProviderParams["reasoning_effort"]; ok {
		t.Fatal("reasoning_effort should not be added by default")
	}
}

func TestValidateAIFileConfigRejectsDuplicateNameProviderForDefaultUpsert(t *testing.T) {
	cfg := aiFileConfig{Version: 1, LLMs: []aiFileLLM{
		{ID: "one", DisplayName: "One", Name: "same-model", Provider: "vllm", URL: "http://example.test/v1/chat/completions"},
		{ID: "two", DisplayName: "Two", Name: "same-model", Provider: "vllm", URL: "http://example.test/v1/chat/completions"},
	}}

	err := validateAIFileConfig(cfg)
	if err == nil {
		t.Fatal("expected duplicate name/provider error")
	}
	if !strings.Contains(err.Error(), "duplicates name/provider") {
		t.Fatalf("error = %v, want duplicate name/provider", err)
	}
}

func TestValidateAIFileConfigAllowsDuplicateNameProviderWhenExplicitCreate(t *testing.T) {
	cfg := aiFileConfig{Version: 1, LLMs: []aiFileLLM{
		{ID: "one", DisplayName: "One", Name: "same-model", Provider: "vllm", URL: "http://example.test/v1/chat/completions", Upsert: aiFileUpsert{Match: "none"}},
		{ID: "two", DisplayName: "Two", Name: "same-model", Provider: "vllm", URL: "http://example.test/v1/chat/completions", Upsert: aiFileUpsert{Match: "none"}},
	}}

	if err := validateAIFileConfig(cfg); err != nil {
		t.Fatalf("validateAIFileConfig returned error: %v", err)
	}
}

func TestResolveAIFileAPIKeyPrecedence(t *testing.T) {
	getenv := func(name string) string {
		if name == "AI_KEY" {
			return "from-env"
		}
		return ""
	}
	readOP := func(ref string) (string, error) {
		if ref != "op://vault/item/field" {
			t.Fatalf("readOP ref = %q", ref)
		}
		return "from-ref", nil
	}

	key, source, err := resolveAIFileAPIKey(aiFileLLM{
		APIKeyRef: "op://vault/item/field",
		APIKeyEnv: "AI_KEY",
		APIKey:    "literal",
	}, getenv, readOP)
	if err != nil {
		t.Fatalf("resolve ref returned error: %v", err)
	}
	if key != "from-ref" || source != "api_key_ref" {
		t.Fatalf("ref precedence = %q/%q, want from-ref/api_key_ref", key, source)
	}

	key, source, err = resolveAIFileAPIKey(aiFileLLM{APIKeyEnv: "AI_KEY", APIKey: "literal"}, getenv, readOP)
	if err != nil {
		t.Fatalf("resolve env returned error: %v", err)
	}
	if key != "from-env" || source != "api_key_env" {
		t.Fatalf("env precedence = %q/%q, want from-env/api_key_env", key, source)
	}

	key, source, err = resolveAIFileAPIKey(aiFileLLM{APIKey: "literal"}, getenv, readOP)
	if err != nil {
		t.Fatalf("resolve literal returned error: %v", err)
	}
	if key != "literal" || source != "api_key" {
		t.Fatalf("literal = %q/%q, want literal/api_key", key, source)
	}
}

func TestResolveAIFileAPIKeyDoesNotLeakLiteralOnMissingEnv(t *testing.T) {
	const literalSecret = "literal-secret-should-not-leak"
	_, _, err := resolveAIFileAPIKey(aiFileLLM{APIKeyEnv: "MISSING_AI_KEY", APIKey: literalSecret}, func(string) string { return "" }, func(string) (string, error) {
		return "", errors.New("should not be called")
	})
	if err == nil {
		t.Fatal("expected missing environment variable error")
	}
	if strings.Contains(err.Error(), literalSecret) {
		t.Fatalf("error leaks literal secret: %v", err)
	}
	if !strings.Contains(err.Error(), "MISSING_AI_KEY") {
		t.Fatalf("error = %v, want environment variable name", err)
	}
}

func TestFindAIFileExistingLLMByNameProvider(t *testing.T) {
	models := []ai.LLMModel{
		{ID: 1, Name: "Qwen/Qwen3.5-122B-A10B", Provider: "open_ai", DisplayName: "Wrong Provider"},
		{ID: 7, Name: "Qwen/Qwen3.5-122B-A10B", Provider: "vllm", DisplayName: "CDCK Qwen"},
	}
	input := discourse.CreateLLMInput{Name: "Qwen/Qwen3.5-122B-A10B", Provider: "vllm", DisplayName: "New Name"}

	model, err := findAIFileExistingLLM(models, "", input)
	if err != nil {
		t.Fatalf("findAIFileExistingLLM returned error: %v", err)
	}
	if model == nil || model.ID != 7 {
		t.Fatalf("matched model = %#v, want id 7", model)
	}

	model, err = findAIFileExistingLLM(models, "none", input)
	if err != nil {
		t.Fatalf("none match returned error: %v", err)
	}
	if model != nil {
		t.Fatalf("none match returned %#v, want nil", model)
	}
}

type fakeAIFileClient struct {
	state            ai.LLMState
	calls            []string
	testErr          error
	settingErr       error
	testInput        discourse.CreateLLMInput
	createInput      discourse.CreateLLMInput
	createdSecret    string
	createdSecretVal string
}

func (f *fakeAIFileClient) record(call string) {
	f.calls = append(f.calls, call)
}

func (f *fakeAIFileClient) FetchState(ctx context.Context) (ai.LLMState, error) {
	f.record("fetch-state")
	return f.state, nil
}

func (f *fakeAIFileClient) CreateModel(ctx context.Context, input discourse.CreateLLMInput) (int64, error) {
	f.record("create-model")
	f.createInput = input
	return 42, nil
}

func (f *fakeAIFileClient) UpdateModel(ctx context.Context, id int64, input discourse.CreateLLMInput) error {
	f.record("update-model")
	return nil
}

func (f *fakeAIFileClient) DeleteModel(ctx context.Context, id int64) error { return nil }

func (f *fakeAIFileClient) TestModel(ctx context.Context, input discourse.CreateLLMInput) error {
	f.record("test-model")
	f.testInput = input
	return f.testErr
}

func (f *fakeAIFileClient) SetDefaultLLM(ctx context.Context, id int64) error { return nil }

func (f *fakeAIFileClient) EnableFeatures(ctx context.Context, settings []string, env map[string]string) error {
	return nil
}

func (f *fakeAIFileClient) CreateAiSecret(ctx context.Context, name, secret string) (int64, error) {
	f.record("create-secret")
	f.createdSecret = name
	f.createdSecretVal = secret
	return 123, nil
}

func (f *fakeAIFileClient) UpdateAiSecret(ctx context.Context, id int64, secret string) error {
	f.record("update-secret")
	f.createdSecretVal = secret
	return nil
}

func (f *fakeAIFileClient) SetSiteSetting(name string, value interface{}) error {
	f.record("set-site-setting")
	if f.settingErr != nil {
		return f.settingErr
	}
	return nil
}

func TestApplyAIFileConfigTestsBeforePersistingSecret(t *testing.T) {
	client := &fakeAIFileClient{}
	cfg := aiFileConfig{Version: 1, LLMs: []aiFileLLM{{
		ID:           "model",
		DisplayName:  "Model",
		Name:         "provider/model",
		URL:          "http://example.test/v1/chat/completions",
		APIKey:       "candidate-secret",
		AiSecretName: "Model API Key",
		Test:         true,
	}}}

	var out bytes.Buffer
	if err := applyAIFileConfig(context.Background(), &out, client, cfg); err != nil {
		t.Fatalf("applyAIFileConfig returned error: %v", err)
	}

	wantCalls := []string{"fetch-state", "test-model", "create-secret", "create-model"}
	if !reflect.DeepEqual(client.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", client.calls, wantCalls)
	}
	if client.testInput.APIKey != "candidate-secret" || client.testInput.AiSecretID != 0 {
		t.Fatalf("test input secret = api_key %q, ai_secret_id %d; want inline key and no secret id", client.testInput.APIKey, client.testInput.AiSecretID)
	}
	if client.createdSecret != "Model API Key" || client.createdSecretVal != "candidate-secret" {
		t.Fatalf("created secret = %q/%q, want Model API Key/candidate-secret", client.createdSecret, client.createdSecretVal)
	}
	if client.createInput.APIKey != "" || client.createInput.AiSecretID != 123 {
		t.Fatalf("create input secret = api_key %q, ai_secret_id %d; want empty/123", client.createInput.APIKey, client.createInput.AiSecretID)
	}
}

func TestApplyAIFileConfigDoesNotPersistSecretWhenTestFails(t *testing.T) {
	client := &fakeAIFileClient{testErr: errors.New("server rejected candidate-secret")}
	cfg := aiFileConfig{Version: 1, LLMs: []aiFileLLM{{
		ID:           "model",
		DisplayName:  "Model",
		Name:         "provider/model",
		URL:          "http://example.test/v1/chat/completions",
		APIKey:       "candidate-secret",
		AiSecretName: "Model API Key",
		Test:         true,
	}}}

	var out bytes.Buffer
	err := applyAIFileConfig(context.Background(), &out, client, cfg)
	if err == nil {
		t.Fatal("expected test failure")
	}
	if strings.Contains(err.Error(), "candidate-secret") {
		t.Fatalf("error leaks secret: %v", err)
	}
	wantCalls := []string{"fetch-state", "test-model"}
	if !reflect.DeepEqual(client.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", client.calls, wantCalls)
	}
}

func TestApplyAIFileConfigRedactsSecretSiteSettingOnError(t *testing.T) {
	secret := "github-token-secret"
	client := &fakeAIFileClient{settingErr: fmt.Errorf("server echoed %s", secret)}
	cfg := aiFileConfig{Version: 1, SiteSettings: map[string]interface{}{
		"ai_bot_github_access_token": secret,
	}}

	var out bytes.Buffer
	err := applyAIFileConfig(context.Background(), &out, client, cfg)
	if err == nil {
		t.Fatal("expected site setting error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks site setting secret: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error = %v, want redacted marker", err)
	}
}

func TestCompleteAIConfigAliases(t *testing.T) {
	aiDir := filepath.Join(t.TempDir(), "ai")
	if err := os.MkdirAll(aiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aiDir, "cdck_qwen.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aiDir, "notes.txt"), []byte(`ignore`), 0o600); err != nil {
		t.Fatal(err)
	}

	suggestions := completeAIConfigAliases(aiDir, "cd")
	if len(suggestions) != 2 {
		t.Fatalf("suggestions = %#v, want alias and filename", suggestions)
	}
	joined := strings.Join(suggestions, "\n")
	if !strings.Contains(joined, "cdck_qwen\t") {
		t.Fatalf("suggestions = %#v, want cdck_qwen alias", suggestions)
	}
	if !strings.Contains(joined, "cdck_qwen.json\t") {
		t.Fatalf("suggestions = %#v, want cdck_qwen.json filename", suggestions)
	}
}

func TestResolveAIConfigFilePathAlias(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "dv")
	aiDir := filepath.Join(configDir, "ai")
	if err := os.MkdirAll(aiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(aiDir, "cdck_qwen.json")
	if err := os.WriteFile(aliasPath, []byte(`{"version":1,"site_settings":{"discourse_ai_enabled":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	path, err := resolveAIConfigFilePath(configDir, "cdck_qwen")
	if err != nil {
		t.Fatalf("resolve alias without extension returned error: %v", err)
	}
	if path != aliasPath {
		t.Fatalf("path = %q, want %q", path, aliasPath)
	}

	path, err = resolveAIConfigFilePath(configDir, "cdck_qwen.json")
	if err != nil {
		t.Fatalf("resolve alias with extension returned error: %v", err)
	}
	if path != aliasPath {
		t.Fatalf("path = %q, want %q", path, aliasPath)
	}
}
