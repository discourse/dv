package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"dv/internal/ai"
	"dv/internal/discourse"
)

// Decision models are Discourse's classification models, separate from LLMs.
type aiFileDecisionModel struct {
	ID            string                          `json:"id"`
	DisplayName   string                          `json:"display_name"`
	Name          string                          `json:"name"`
	Provider      string                          `json:"provider"`
	URL           string                          `json:"url"`
	APIKey        string                          `json:"api_key"`
	APIKeyRef     string                          `json:"api_key_ref"`
	APIKeyEnv     string                          `json:"api_key_env"`
	AiSecretID    int64                           `json:"ai_secret_id"`
	AiSecretName  string                          `json:"ai_secret_name"`
	VisionEnabled bool                            `json:"vision_enabled"`
	CustomHeaders []discourse.DecisionModelHeader `json:"custom_headers"`
	Test          bool                            `json:"test"`
	Upsert        aiFileUpsert                    `json:"upsert"`
}

func (m aiFileDecisionModel) credentials() aiFileLLM {
	return aiFileLLM{APIKey: m.APIKey, APIKeyRef: m.APIKeyRef, APIKeyEnv: m.APIKeyEnv, AiSecretID: m.AiSecretID, AiSecretName: m.AiSecretName}
}

func (m aiFileDecisionModel) input() discourse.DecisionModel {
	return discourse.DecisionModel{DisplayName: strings.TrimSpace(m.DisplayName), Name: strings.TrimSpace(m.Name), Provider: strings.TrimSpace(m.Provider), URL: strings.TrimSpace(m.URL), AiSecretID: m.AiSecretID, VisionEnabled: m.VisionEnabled, CustomHeaders: m.CustomHeaders}
}

func validateAIFileDecisionModels(models []aiFileDecisionModel, seenIDs map[string]struct{}) error {
	modes := make([]string, len(models))
	for i, model := range models {
		input := model.input()
		if input.DisplayName == "" || input.Name == "" {
			return fmt.Errorf("decision_models[%d]: display_name and name are required", i)
		}
		switch input.Provider {
		case "system_one", "typesafe", "open_ai":
		default:
			return fmt.Errorf("decision_models[%d]: unsupported provider %q (use system_one, typesafe, or open_ai)", i, input.Provider)
		}
		u, err := url.Parse(input.URL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "srv") || (u.Scheme == "srv" && u.Port() != "") {
			return fmt.Errorf("decision_models[%d]: url must be an http, https, or srv endpoint (srv cannot include a port)", i)
		}
		if id := strings.ToLower(strings.TrimSpace(model.ID)); id != "" {
			if _, ok := seenIDs[id]; ok {
				return fmt.Errorf("decision_models[%d]: duplicate id %q", i, model.ID)
			}
			seenIDs[id] = struct{}{}
		}
		mode, err := normalizeAIFileUpsertMatch(model.Upsert.Match)
		if err != nil {
			return fmt.Errorf("decision_models[%d]: %w", i, err)
		}
		modes[i] = mode
		for j := 0; j < i; j++ {
			previous := models[j].input()
			sameName := strings.EqualFold(input.Name, previous.Name)
			duplicate := (sameName && strings.EqualFold(input.Provider, previous.Provider) && (mode == "name_provider" || modes[j] == "name_provider")) ||
				(sameName && (mode == "name" || modes[j] == "name")) ||
				(strings.EqualFold(input.DisplayName, previous.DisplayName) && (mode == "display_name" || modes[j] == "display_name"))
			if duplicate {
				return fmt.Errorf("decision_models[%d]: duplicates upsert match with decision_models[%d]; use unique values or upsert.match \"none\" for intentional duplicate creation", i, j)
			}
		}
	}
	return nil
}

type aiFileDecisionClient interface {
	FetchDecisionModels(context.Context) (discourse.DecisionModelState, error)
	SaveDecisionModel(context.Context, int64, discourse.DecisionModel) (int64, error)
	TestDecisionModel(context.Context, discourse.DecisionModel) error
}

func applyAIFileDecisionModels(ctx context.Context, out io.Writer, client aiFileDiscourseClient, models []aiFileDecisionModel, keys []string, modelIDs map[string]int64) error {
	if len(models) == 0 {
		return nil
	}
	dc, ok := client.(aiFileDecisionClient)
	if !ok {
		return fmt.Errorf("client does not support decision models")
	}
	for i, model := range models {
		state, err := dc.FetchDecisionModels(ctx)
		if err != nil {
			return fmt.Errorf("fetch decision models: %w", err)
		}
		input := model.input()
		candidates := make([]ai.LLMModel, len(state.Models))
		for j, existing := range state.Models {
			candidates[j] = ai.LLMModel{ID: existing.ID, DisplayName: existing.DisplayName, Name: existing.Name, Provider: existing.Provider, AiSecretID: existing.AiSecretID}
		}
		existing, err := findAIFileExistingLLM(candidates, model.Upsert.Match, discourse.CreateLLMInput{DisplayName: input.DisplayName, Name: input.Name, Provider: input.Provider})
		if err != nil {
			return err
		}
		var id int64
		credentials := discourse.CreateLLMInput{}
		if existing != nil {
			if existing.ID < 0 {
				return fmt.Errorf("decision model %s: cannot update a seeded model", input.DisplayName)
			}
			id = existing.ID
			credentials.ExistingAiSecretID = existing.AiSecretID
		}
		// The decision API accepts only ai_secret_id, so credentials must be
		// persisted before testing (unlike the LLM API's candidate raw key).
		secretConfig := model.credentials()
		if secretConfig.AiSecretID == 0 && strings.TrimSpace(secretConfig.AiSecretName) == "" && keys[i] != "" {
			if existing != nil && existing.AiSecretID > 0 {
				secretConfig.AiSecretID = existing.AiSecretID
			} else {
				secretConfig.AiSecretName = input.DisplayName + " API Key"
			}
		}
		credentials, err = prepareAIFileCredentials(ctx, client, ai.LLMState{Meta: ai.LLMMetadata{AiSecrets: state.AiSecrets}}, secretConfig, credentials, keys[i])
		if err != nil {
			return fmt.Errorf("decision model %s: %w", input.DisplayName, err)
		}
		input.AiSecretID = credentials.AiSecretID
		if model.Test {
			fmt.Fprintf(out, "Testing decision model %s...\n", input.DisplayName)
			if err := dc.TestDecisionModel(ctx, input); err != nil {
				return fmt.Errorf("decision model %s: test failed: %w", input.DisplayName, err)
			}
		}
		id, err = dc.SaveDecisionModel(ctx, id, input)
		if err != nil {
			return fmt.Errorf("save decision model %s: %w", input.DisplayName, err)
		}
		recordAIFileModelID(modelIDs, aiFileLLM{ID: model.ID, DisplayName: input.DisplayName, Name: input.Name}, id)
		fmt.Fprintf(out, "Saved decision model %s (id %d).\n", input.DisplayName, id)
	}
	return nil
}
