package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dv/internal/ai"
	"dv/internal/discourse"
)

type fakeDecisionFileClient struct {
	fakeAIFileClient
	decisionState   discourse.DecisionModelState
	saved           discourse.DecisionModel
	savedID         int64
	decisionTestErr error
	settings        map[string]interface{}
}

func (f *fakeDecisionFileClient) FetchDecisionModels(context.Context) (discourse.DecisionModelState, error) {
	f.record("fetch-decisions")
	return f.decisionState, nil
}
func (f *fakeDecisionFileClient) TestDecisionModel(_ context.Context, input discourse.DecisionModel) error {
	f.record("test-decision")
	if input.AiSecretID == 0 {
		return errors.New("missing secret for test")
	}
	return f.decisionTestErr
}
func (f *fakeDecisionFileClient) SaveDecisionModel(_ context.Context, id int64, input discourse.DecisionModel) (int64, error) {
	f.record("save-decision")
	f.saved, f.savedID = input, id
	if id == 0 {
		id = 42
	}
	return id, nil
}

func decisionFixture() aiFileDecisionModel {
	return aiFileDecisionModel{ID: "snorlax", DisplayName: "Snorlax d3", Name: "d3", Provider: "system_one", URL: "https://snorlax.example:13500/v1/systemone", APIKey: "test-classification-key", VisionEnabled: true, Test: true}
}

func TestDecodeDecisionOnlyConfig(t *testing.T) {
	cfg := aiFileConfig{DecisionModels: []aiFileDecisionModel{decisionFixture()}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeAIFileConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.DecisionModels) != 1 || got.DecisionModels[0].Name != "d3" {
		t.Fatalf("config = %#v", got)
	}
}

func TestValidateDecisionModels(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*aiFileDecisionModel)
	}{
		{"missing name", func(m *aiFileDecisionModel) { m.Name = "" }},
		{"missing display name", func(m *aiFileDecisionModel) { m.DisplayName = "" }},
		{"bad provider", func(m *aiFileDecisionModel) { m.Provider = "vllm" }},
		{"bad url", func(m *aiFileDecisionModel) { m.URL = "ftp://example.test" }},
		{"srv port", func(m *aiFileDecisionModel) { m.URL = "srv://example.test:13500" }},
		{"bad upsert", func(m *aiFileDecisionModel) { m.Upsert.Match = "oops" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := decisionFixture()
			tt.mutate(&m)
			if err := validateAIFileConfig(aiFileConfig{DecisionModels: []aiFileDecisionModel{m}}); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	m := decisionFixture()
	if err := validateAIFileConfig(aiFileConfig{DecisionModels: []aiFileDecisionModel{m, m}}); err == nil {
		t.Fatal("expected duplicate error")
	}
	m.ID = "other"
	if err := validateAIFileConfig(aiFileConfig{DecisionModels: []aiFileDecisionModel{decisionFixture(), m}}); err == nil {
		t.Fatal("expected duplicate match error")
	}
}

func TestApplyDecisionModelCreatesSecretAndTestsBeforeSave(t *testing.T) {
	client := &fakeDecisionFileClient{}
	var out bytes.Buffer
	if err := applyAIFileConfig(context.Background(), &out, client, aiFileConfig{DecisionModels: []aiFileDecisionModel{decisionFixture()}}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(client.calls, ","); got != "fetch-decisions,create-secret,test-decision,save-decision" {
		t.Fatalf("calls = %s", got)
	}
	if client.saved.Name != "d3" || client.saved.AiSecretID != 123 || !client.saved.VisionEnabled || client.savedID != 0 {
		t.Fatalf("saved = %#v, id %d", client.saved, client.savedID)
	}
	if strings.Contains(out.String(), "test-classification-key") {
		t.Fatal("output leaks secret")
	}
}

func TestApplyDecisionModelUpdatesExistingAndReusesSecret(t *testing.T) {
	client := &fakeDecisionFileClient{decisionState: discourse.DecisionModelState{Models: []discourse.DecisionModel{{ID: 7, Name: "d3", Provider: "system_one", AiSecretID: 99}}}}
	m := decisionFixture()
	if err := applyAIFileConfig(context.Background(), &bytes.Buffer{}, client, aiFileConfig{DecisionModels: []aiFileDecisionModel{m}}); err != nil {
		t.Fatal(err)
	}
	if client.savedID != 7 || client.saved.AiSecretID != 99 {
		t.Fatalf("saved id %d, secret %d", client.savedID, client.saved.AiSecretID)
	}
	if got := strings.Join(client.calls, ","); got != "fetch-decisions,update-secret,test-decision,save-decision" {
		t.Fatalf("calls = %s", got)
	}
}

func TestApplyDecisionModelNamedSecretWithoutKey(t *testing.T) {
	client := &fakeDecisionFileClient{decisionState: discourse.DecisionModelState{AiSecrets: []ai.AiSecret{{ID: 88, Name: "Shared key"}}}}
	m := decisionFixture()
	m.APIKey = ""
	m.AiSecretName = "Shared key"
	if err := applyAIFileConfig(context.Background(), &bytes.Buffer{}, client, aiFileConfig{DecisionModels: []aiFileDecisionModel{m}}); err != nil {
		t.Fatal(err)
	}
	if client.saved.AiSecretID != 88 {
		t.Fatalf("secret = %d", client.saved.AiSecretID)
	}
}

func TestApplyDecisionModelTestFailureRedactsAndDoesNotSave(t *testing.T) {
	m := decisionFixture()
	client := &fakeDecisionFileClient{decisionTestErr: errors.New("bad key " + m.APIKey)}
	err := applyAIFileConfig(context.Background(), &bytes.Buffer{}, client, aiFileConfig{DecisionModels: []aiFileDecisionModel{m}})
	if err == nil || strings.Contains(err.Error(), m.APIKey) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(strings.Join(client.calls, ","), "save-decision") {
		t.Fatal("saved failed model")
	}
}

func TestApplyDecisionModelSeededCannotUpdate(t *testing.T) {
	client := &fakeDecisionFileClient{decisionState: discourse.DecisionModelState{Models: []discourse.DecisionModel{{ID: -1, Name: "d3", Provider: "system_one"}}}}
	err := applyAIFileConfig(context.Background(), &bytes.Buffer{}, client, aiFileConfig{DecisionModels: []aiFileDecisionModel{decisionFixture()}})
	if err == nil || !strings.Contains(err.Error(), "seeded") {
		t.Fatalf("error = %v", err)
	}
	if got := strings.Join(client.calls, ","); got != "fetch-decisions" {
		t.Fatalf("calls = %s", got)
	}
}

func (f *fakeDecisionFileClient) SetSiteSetting(name string, value interface{}) error {
	if f.settings == nil {
		f.settings = map[string]interface{}{}
	}
	f.settings[name] = value
	return f.fakeAIFileClient.SetSiteSetting(name, value)
}

func TestApplyDecisionModelSiteSettingReference(t *testing.T) {
	client := &fakeDecisionFileClient{}
	cfg := aiFileConfig{DecisionModels: []aiFileDecisionModel{decisionFixture()}, SiteSettings: map[string]interface{}{"ai_ask_ai_search_intent_decision_model": "@snorlax"}}
	if err := applyAIFileConfig(context.Background(), &bytes.Buffer{}, client, cfg); err != nil {
		t.Fatal(err)
	}
	if got := client.settings["ai_ask_ai_search_intent_decision_model"]; got != int64(42) {
		t.Fatalf("setting = %#v, want 42", got)
	}
}

func TestValidateDecisionModelIDConflictsWithLLM(t *testing.T) {
	cfg := aiFileConfig{DecisionModels: []aiFileDecisionModel{decisionFixture()}, LLMs: []aiFileLLM{{ID: "SNORLAX", DisplayName: "Chat", Name: "chat", URL: "https://example.test/v1/chat/completions"}}}
	if err := validateAIFileConfig(cfg); err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("error = %v", err)
	}
}
