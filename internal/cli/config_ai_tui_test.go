package cli

import (
	"context"
	"strings"
	"testing"

	"dv/internal/ai"
	"dv/internal/discourse"
)

type fakeAIConfigClient struct {
	createdName   string
	createdSecret string
	createdID     int64
	updatedID     int64
	updatedSecret string
}

func (f *fakeAIConfigClient) FetchState(ctx context.Context) (ai.LLMState, error) {
	return ai.LLMState{}, nil
}
func (f *fakeAIConfigClient) CreateModel(ctx context.Context, input discourse.CreateLLMInput) (int64, error) {
	return 1, nil
}
func (f *fakeAIConfigClient) UpdateModel(ctx context.Context, id int64, input discourse.CreateLLMInput) error {
	return nil
}
func (f *fakeAIConfigClient) DeleteModel(ctx context.Context, id int64) error { return nil }
func (f *fakeAIConfigClient) TestModel(ctx context.Context, input discourse.CreateLLMInput) error {
	return nil
}
func (f *fakeAIConfigClient) SetDefaultLLM(ctx context.Context, id int64) error { return nil }
func (f *fakeAIConfigClient) EnableFeatures(ctx context.Context, settings []string, env map[string]string) error {
	return nil
}
func (f *fakeAIConfigClient) CreateAiSecret(ctx context.Context, name, secret string) (int64, error) {
	f.createdName = name
	f.createdSecret = secret
	if f.createdID == 0 {
		f.createdID = 123
	}
	return f.createdID, nil
}
func (f *fakeAIConfigClient) UpdateAiSecret(ctx context.Context, id int64, secret string) error {
	f.updatedID = id
	f.updatedSecret = secret
	return nil
}

func TestCatalogItemsHideProvidersWithoutCredentials(t *testing.T) {
	items := catalogItems(ai.ProviderCatalog{Entries: []ai.ProviderEntry{
		{
			ID:             "openrouter",
			Title:          "OpenRouter",
			HasCredentials: false,
			Models: []ai.ProviderModel{{
				ID:          "cached/model",
				DisplayName: "Cached Model",
			}},
		},
		{
			ID:             "venice",
			Title:          "Venice AI",
			HasCredentials: true,
			Models: []ai.ProviderModel{{
				ID:          "venice-uncensored",
				DisplayName: "Venice Uncensored",
			}},
		},
	}})

	if len(items) != 1 {
		t.Fatalf("expected only credentialed provider models, got %d items", len(items))
	}
	item, ok := items[0].(providerItem)
	if !ok {
		t.Fatalf("item type = %T, want providerItem", items[0])
	}
	if item.entryID != "venice" || item.model.ID != "venice-uncensored" {
		t.Fatalf("unexpected item: entryID=%q model=%q", item.entryID, item.model.ID)
	}
}

func TestCatalogItemsLabelVeniceAsVenice(t *testing.T) {
	items := catalogItems(ai.ProviderCatalog{Entries: []ai.ProviderEntry{{
		ID:             "venice",
		Title:          "Venice AI",
		HasCredentials: true,
		Models: []ai.ProviderModel{{
			ID:            "gpt-5.4-mini",
			DisplayName:   "GPT-5.4 Mini",
			Provider:      "open_ai",
			ContextTokens: 400000,
		}},
	}}})

	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0].(providerItem)
	description := item.Description()
	if !strings.Contains(description, "venice_ai") {
		t.Fatalf("Description() = %q, want venice_ai label", description)
	}
	if strings.Contains(description, "open_ai") {
		t.Fatalf("Description() = %q, should not expose open_ai for Venice catalog item", description)
	}
}

func TestDisplayProviderForLLMDetectsVeniceOpenAICompat(t *testing.T) {
	llm := ai.LLMModel{Provider: "open_ai", URL: "https://api.venice.ai/api/v1/chat/completions"}
	if got := displayProviderForLLM(llm); got != "venice_ai" {
		t.Fatalf("displayProviderForLLM = %q, want venice_ai", got)
	}
}

func TestPrepareModelCredentialsCreatesVeniceAiSecret(t *testing.T) {
	client := &fakeAIConfigClient{}
	model := aiConfigModel{}
	payload, err := model.prepareModelCredentials(context.Background(), client, discourse.CreateLLMInput{
		Provider:    "open_ai",
		URL:         "https://api.venice.ai/api/v1/chat/completions",
		APIKey:      "venice-key",
		DisplayName: "GPT-5.4 Mini",
	})
	if err != nil {
		t.Fatalf("prepareModelCredentials: %v", err)
	}
	if client.createdName != "Venice AI API Key" {
		t.Fatalf("createdName = %q, want Venice AI API Key", client.createdName)
	}
	if client.createdSecret != "venice-key" {
		t.Fatalf("createdSecret = %q", client.createdSecret)
	}
	if payload.AiSecretID != 123 || payload.APIKey != "" {
		t.Fatalf("payload secret/id = %d/%q, want 123/empty", payload.AiSecretID, payload.APIKey)
	}
}

func TestPrepareModelCredentialsReusesExistingVeniceAiSecret(t *testing.T) {
	client := &fakeAIConfigClient{}
	model := aiConfigModel{state: ai.LLMState{Meta: ai.LLMMetadata{AiSecrets: []ai.AiSecret{{ID: 77, Name: "Venice AI API Key"}}}}}
	payload, err := model.prepareModelCredentials(context.Background(), client, discourse.CreateLLMInput{
		Provider: "open_ai",
		URL:      "https://api.venice.ai/api/v1/chat/completions",
		APIKey:   "updated-key",
	})
	if err != nil {
		t.Fatalf("prepareModelCredentials: %v", err)
	}
	if client.createdName != "" {
		t.Fatalf("unexpected created secret %q", client.createdName)
	}
	if client.updatedID != 77 || client.updatedSecret != "updated-key" {
		t.Fatalf("updated = %d/%q, want 77/updated-key", client.updatedID, client.updatedSecret)
	}
	if payload.AiSecretID != 77 || payload.APIKey != "" {
		t.Fatalf("payload secret/id = %d/%q, want 77/empty", payload.AiSecretID, payload.APIKey)
	}
}
