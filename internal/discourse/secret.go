package discourse

import (
	"encoding/json"
	"fmt"
)

// CreateAiSecret creates a new named secret and returns its ID.
func (c *Client) CreateAiSecret(name, secret string) (int64, error) {
	payload := map[string]interface{}{
		"ai_secret": map[string]interface{}{
			"name":   name,
			"secret": secret,
		},
	}

	resp, body, err := c.doRequest("POST", "/admin/plugins/discourse-ai/ai-secrets.json", payload)
	if err != nil {
		return 0, err
	}

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return 0, fmt.Errorf("create AiSecret: status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		AiSecret struct {
			ID int64 `json:"id"`
		} `json:"ai_secret"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, fmt.Errorf("decode AiSecret response: %w", err)
	}

	return result.AiSecret.ID, nil
}

// UpdateAiSecret replaces the secret value of an existing AiSecret.
func (c *Client) UpdateAiSecret(id int64, secret string) error {
	payload := map[string]interface{}{
		"ai_secret": map[string]interface{}{
			"secret": secret,
		},
	}

	path := fmt.Sprintf("/admin/plugins/discourse-ai/ai-secrets/%d.json", id)
	resp, body, err := c.doRequest("PUT", path, payload)
	if err != nil {
		return err
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("update AiSecret: status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}
