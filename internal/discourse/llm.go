package discourse

import (
	"encoding/json"
	"fmt"
	"strings"

	"dv/internal/ai"
	"dv/internal/docker"
)

// LLMListResponse is the API response for listing LLMs
type LLMListResponse struct {
	AILLMs []ai.LLMModel  `json:"ai_llms"`
	Meta   ai.LLMMetadata `json:"meta"`
}

// CreateLLMInput captures the attributes for creating/updating an LLM
type CreateLLMInput struct {
	DisplayName        string
	Name               string
	Provider           string
	Tokenizer          string
	URL                string
	APIKey             string
	AiSecretID         int64 // use a pre-created AiSecret instead of raw APIKey
	MaxPromptTokens    int
	MaxOutputTokens    int
	InputCost          float64
	CachedInputCost    float64
	OutputCost         float64
	EnabledChatBot     bool
	VisionEnabled      bool
	ProviderParams     map[string]interface{}
	SetAsDefault       bool
	ExistingID         int64
	ExistingAiSecretID int64
}

// ListLLMs retrieves all configured LLM models
func (c *Client) ListLLMs() ([]ai.LLMModel, ai.LLMMetadata, error) {
	resp, body, err := c.doRequest("GET", "/admin/plugins/discourse-ai/ai-llms.json", nil)
	if err != nil {
		return nil, ai.LLMMetadata{}, err
	}

	if resp.StatusCode != 200 {
		return nil, ai.LLMMetadata{}, fmt.Errorf("list LLMs: status %d: %s", resp.StatusCode, string(body))
	}

	var result LLMListResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, ai.LLMMetadata{}, fmt.Errorf("decode LLMs: %w", err)
	}

	return result.AILLMs, result.Meta, nil
}

// GetDefaultLLMID retrieves the current default LLM ID from site settings
func (c *Client) GetDefaultLLMID() (int64, error) {
	val, err := c.GetSiteSetting("ai_default_llm_model")
	if err != nil {
		return 0, err
	}

	switch v := val.(type) {
	case float64:
		return int64(v), nil
	case int64:
		return v, nil
	case string:
		if v == "" {
			return 0, nil
		}
		var id int64
		fmt.Sscanf(v, "%d", &id)
		return id, nil
	default:
		return 0, nil
	}
}

// FetchState retrieves the complete LLM state (models + metadata + default)
func (c *Client) FetchState() (ai.LLMState, error) {
	var state ai.LLMState

	models, meta, err := c.ListLLMs()
	if err != nil {
		return state, err
	}

	defaultID, err := c.GetDefaultLLMID()
	if err != nil {
		c.verboseLog("Warning: failed to get default LLM ID: %v", err)
		// Non-fatal, continue with 0
	}

	state.Models = models
	state.Meta = meta
	state.DefaultID = defaultID

	return state, nil
}

// CreateLLM creates a new LLM model
func (c *Client) CreateLLM(input CreateLLMInput) (int64, error) {
	payload := buildLLMPayload(input)

	resp, body, err := c.doRequest("POST", "/admin/plugins/discourse-ai/ai-llms.json", payload)
	if err != nil {
		return 0, err
	}

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return 0, fmt.Errorf("create LLM: status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		AILLM struct {
			ID int64 `json:"id"`
		} `json:"ai_llm"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("decode create response: %w", err)
	}

	// Make the new LLM available to the AI bot by default.
	if result.AILLM.ID > 0 {
		if err := c.AppendAIBotEnabledLLM(result.AILLM.ID); err != nil {
			c.verboseLog("Warning: failed to append ai_bot_enabled_llms: %v", err)
		}
	}

	// Set as default if requested
	if input.SetAsDefault && result.AILLM.ID > 0 {
		if err := c.SetDefaultLLM(result.AILLM.ID); err != nil {
			c.verboseLog("Warning: failed to set as default: %v", err)
		}
	}

	// Toggle companion user (bot)
	if input.EnabledChatBot && result.AILLM.ID > 0 {
		c.toggleCompanionUser(result.AILLM.ID, true)
	}

	return result.AILLM.ID, nil
}

// UpdateLLM updates an existing LLM model
func (c *Client) UpdateLLM(id int64, input CreateLLMInput) error {
	payload := buildLLMPayload(input)

	path := fmt.Sprintf("/admin/plugins/discourse-ai/ai-llms/%d.json", id)
	resp, body, err := c.doRequest("PUT", path, payload)
	if err != nil {
		return err
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("update LLM: status %d: %s", resp.StatusCode, string(body))
	}

	// Set as default if requested
	if input.SetAsDefault {
		if err := c.SetDefaultLLM(id); err != nil {
			c.verboseLog("Warning: failed to set as default: %v", err)
		}
	}

	// Toggle companion user based on chat bot setting
	c.toggleCompanionUser(id, input.EnabledChatBot)

	return nil
}

// DeleteLLM removes an LLM model
func (c *Client) DeleteLLM(id int64) error {
	// First disable the companion user
	c.toggleCompanionUser(id, false)

	path := fmt.Sprintf("/admin/plugins/discourse-ai/ai-llms/%d.json", id)
	resp, body, err := c.doRequest("DELETE", path, nil)
	if err != nil {
		return err
	}

	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		return fmt.Errorf("delete LLM: status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// TestLLM validates an LLM configuration by making a test request
func (c *Client) TestLLM(input CreateLLMInput) error {
	payload := buildLLMPayload(input)

	resp, body, err := c.doRequest("POST", "/admin/plugins/discourse-ai/ai-llms/test.json", payload)
	if err != nil {
		return err
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("test LLM: status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// SetDefaultLLM sets the default LLM model
func (c *Client) SetDefaultLLM(id int64) error {
	return c.SetSiteSetting("ai_default_llm_model", id)
}

// toggleCompanionUser enables or disables the AI bot user for an LLM
// This is done via Rails since there's no direct API endpoint
func (c *Client) toggleCompanionUser(id int64, enable bool) {
	if !docker.Running(c.ContainerName) {
		return
	}

	script := fmt.Sprintf(`
llm = LlmModel.find_by(id: %d)
exit 0 unless llm
llm.enabled_chat_bot = %t
llm.toggle_companion_user
`, id, enable)

	cmd := fmt.Sprintf("cd %s && RAILS_ENV=development bundle exec rails runner - <<'RUBY'\n%s\nRUBY",
		shellQuote(c.Workdir), script)
	docker.ExecOutput(c.ContainerName, c.Workdir, c.Envs, []string{"bash", "-lc", cmd})
}

func buildLLMPayload(input CreateLLMInput) map[string]interface{} {
	payload := map[string]interface{}{
		"ai_llm": map[string]interface{}{
			"display_name":      strings.TrimSpace(input.DisplayName),
			"name":              strings.TrimSpace(input.Name),
			"provider":          strings.TrimSpace(input.Provider),
			"tokenizer":         strings.TrimSpace(input.Tokenizer),
			"url":               strings.TrimSpace(input.URL),
			"max_prompt_tokens": input.MaxPromptTokens,
			"max_output_tokens": input.MaxOutputTokens,
			"input_cost":        input.InputCost,
			"cached_input_cost": input.CachedInputCost,
			"output_cost":       input.OutputCost,
			"enabled_chat_bot":  input.EnabledChatBot,
			"vision_enabled":    input.VisionEnabled,
		},
	}

	llm := payload["ai_llm"].(map[string]interface{})
	if input.AiSecretID > 0 {
		llm["ai_secret_id"] = input.AiSecretID
	} else if apiKey := strings.TrimSpace(input.APIKey); apiKey != "" {
		llm["api_key"] = apiKey
		if input.ExistingAiSecretID > 0 {
			llm["ai_secret_id"] = nil // clear old AiSecret reference so inline key takes effect
		}
	}
	if input.ProviderParams != nil {
		llm["provider_params"] = input.ProviderParams
	}

	return payload
}
