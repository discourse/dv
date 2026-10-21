package discourse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"dv/internal/ai"
)

// DecisionModelHeader is a custom HTTP header for a decision model endpoint.
type DecisionModelHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// DecisionModel is a Discourse AI decision model configuration.
type DecisionModel struct {
	ID            int64                 `json:"id"`
	DisplayName   string                `json:"display_name"`
	Name          string                `json:"name"`
	Provider      string                `json:"provider"`
	URL           string                `json:"url"`
	AiSecretID    int64                 `json:"ai_secret_id"`
	VisionEnabled bool                  `json:"vision_enabled"`
	CustomHeaders []DecisionModelHeader `json:"custom_headers"`
}

// DecisionModelState includes the models and available named credentials.
type DecisionModelState struct {
	Models    []DecisionModel
	AiSecrets []ai.AiSecret
}

// FetchDecisionModels retrieves all decision models and available named credentials.
func (c *ClientWrapper) FetchDecisionModels(ctx context.Context) (DecisionModelState, error) {
	if err := c.Client.EnsureAPIKey(); err != nil {
		return DecisionModelState{}, err
	}
	return c.Client.fetchDecisionModels()
}

// SaveDecisionModel creates a model when id is zero, or updates the specified model.
func (c *ClientWrapper) SaveDecisionModel(ctx context.Context, id int64, input DecisionModel) (int64, error) {
	if err := c.Client.EnsureAPIKey(); err != nil {
		return 0, err
	}
	return c.Client.saveDecisionModel(id, input)
}

// TestDecisionModel validates a decision model configuration against its endpoint.
func (c *ClientWrapper) TestDecisionModel(ctx context.Context, input DecisionModel) error {
	if err := c.Client.EnsureAPIKey(); err != nil {
		return err
	}
	return c.Client.testDecisionModel(input)
}

func (c *Client) fetchDecisionModels() (DecisionModelState, error) {
	resp, body, err := c.doRequest(http.MethodGet, "/admin/plugins/discourse-ai/ai-decision-models.json", nil)
	if err != nil {
		return DecisionModelState{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return DecisionModelState{}, fmt.Errorf("list decision models: status %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Models []DecisionModel `json:"ai_decision_models"`
		Meta   struct {
			AiSecrets []ai.AiSecret `json:"ai_secrets"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return DecisionModelState{}, fmt.Errorf("decode decision models: %w", err)
	}
	return DecisionModelState{Models: result.Models, AiSecrets: result.Meta.AiSecrets}, nil
}

func (c *Client) saveDecisionModel(id int64, input DecisionModel) (int64, error) {
	method, path := http.MethodPost, "/admin/plugins/discourse-ai/ai-decision-models.json"
	if id != 0 {
		method = http.MethodPut
		path = fmt.Sprintf("/admin/plugins/discourse-ai/ai-decision-models/%d.json", id)
	}
	resp, body, err := c.doRequest(method, path, buildDecisionModelPayload(input))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return 0, fmt.Errorf("save decision model: status %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Model DecisionModel `json:"ai_decision_model"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("decode save decision model response: %w", err)
	}
	if result.Model.ID == 0 {
		return 0, fmt.Errorf("save decision model: response missing model ID")
	}
	return result.Model.ID, nil
}

func (c *Client) testDecisionModel(input DecisionModel) error {
	resp, body, err := c.doRequest(http.MethodPost, "/admin/plugins/discourse-ai/ai-decision-models/test.json", buildDecisionModelPayload(input))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("test decision model: status %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("decode test decision model response: %w", err)
	}
	if !result.Success {
		if result.Error != "" {
			return fmt.Errorf("test decision model: %s", result.Error)
		}
		return fmt.Errorf("test decision model: response did not report success")
	}
	return nil
}

func buildDecisionModelPayload(input DecisionModel) map[string]interface{} {
	headers := input.CustomHeaders
	if headers == nil {
		headers = []DecisionModelHeader{}
	}
	// A zero ID means no credential; send null to clear any existing reference.
	var secretID interface{}
	if input.AiSecretID != 0 {
		secretID = input.AiSecretID
	}
	return map[string]interface{}{
		"ai_decision_model": map[string]interface{}{
			"display_name":   input.DisplayName,
			"name":           input.Name,
			"provider":       input.Provider,
			"url":            input.URL,
			"ai_secret_id":   secretID,
			"vision_enabled": input.VisionEnabled,
			"custom_headers": headers,
		},
	}
}
