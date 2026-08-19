package discourse

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// SiteSetting represents a Discourse site setting
type SiteSetting struct {
	Setting     string      `json:"setting"`
	Value       interface{} `json:"value"`
	Default     interface{} `json:"default"`
	Description string      `json:"description"`
	Type        string      `json:"type"`
}

// SiteSettingsResponse is the API response for site settings
type SiteSettingsResponse struct {
	SiteSettings []SiteSetting `json:"site_settings"`
}

// GetSiteSetting retrieves a single site setting by name
func (c *Client) GetSiteSetting(name string) (interface{}, error) {
	path := fmt.Sprintf("/admin/site_settings.json?filter=%s", url.QueryEscape(name))
	resp, body, err := c.doRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("get setting %s: status %d: %s", name, resp.StatusCode, string(body))
	}

	var result SiteSettingsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode settings: %w", err)
	}

	for _, s := range result.SiteSettings {
		if s.Setting == name {
			return s.Value, nil
		}
	}

	return nil, fmt.Errorf("setting %s not found", name)
}

// SetSiteSetting updates a site setting value
func (c *Client) SetSiteSetting(name string, value interface{}) error {
	path := fmt.Sprintf("/admin/site_settings/%s.json", url.PathEscape(name))
	payload := map[string]interface{}{
		name: value,
	}

	resp, body, err := c.doRequest("PUT", path, payload)
	if err != nil {
		return err
	}

	// Accept 200 OK and 204 No Content as success
	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		return fmt.Errorf("set setting %s: status %d: %s", name, resp.StatusCode, string(body))
	}

	return nil
}

// EnableSiteSettings enables multiple boolean site settings.
// It always enables discourse_ai_enabled first as a prerequisite.
func (c *Client) EnableSiteSettings(names []string) error {
	// Always enable the main AI plugin first
	if err := c.SetSiteSetting("discourse_ai_enabled", true); err != nil {
		c.verboseLog("Warning: failed to enable discourse_ai_enabled: %v", err)
	}

	for _, name := range names {
		if err := c.SetSiteSetting(name, true); err != nil {
			c.verboseLog("Warning: failed to enable %s: %v", name, err)
			// Continue with other settings
		}
	}
	return nil
}

// GetSiteSettingBool retrieves a boolean site setting
func (c *Client) GetSiteSettingBool(name string) (bool, error) {
	val, err := c.GetSiteSetting(name)
	if err != nil {
		return false, err
	}

	switch v := val.(type) {
	case bool:
		return v, nil
	case string:
		return v == "true" || v == "t" || v == "1", nil
	default:
		return false, fmt.Errorf("setting %s is not a boolean: %T", name, val)
	}
}

// GetSiteSettingString retrieves a string site setting
func (c *Client) GetSiteSettingString(name string) (string, error) {
	val, err := c.GetSiteSetting(name)
	if err != nil {
		return "", err
	}

	switch v := val.(type) {
	case string:
		return v, nil
	case float64:
		return fmt.Sprintf("%v", v), nil
	default:
		return fmt.Sprintf("%v", val), nil
	}
}

func parseInt64ListSetting(val interface{}) []int64 {
	var ids []int64
	seen := map[int64]struct{}{}

	add := func(id int64) {
		if id <= 0 {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	switch v := val.(type) {
	case nil:
		return nil
	case float64:
		add(int64(v))
	case int64:
		add(v)
	case string:
		s := strings.TrimSpace(v)
		if s == "" || s == "null" || s == "[]" {
			return nil
		}
		parts := strings.FieldsFunc(s, func(r rune) bool {
			return r == '|' || r == ',' || r == ' ' || r == '\n' || r == '\t'
		})
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			id, err := strconv.ParseInt(p, 10, 64)
			if err != nil {
				continue
			}
			add(id)
		}
	case []interface{}:
		for _, raw := range v {
			switch t := raw.(type) {
			case float64:
				add(int64(t))
			case int64:
				add(t)
			case string:
				id, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
				if err != nil {
					continue
				}
				add(id)
			}
		}
	}

	return ids
}

func formatInt64ListSetting(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, "|")
}

const (
	discourseStaffGroupID       int64 = 3
	discourseTrustLevel0GroupID int64 = 10
)

// EnsureAIBotDebuggingAllowedGroups ensures the staff group can access AI bot
// debugging. When the setting is empty, it preserves dv's existing behavior of
// also allowing trust-level-0 users.
func (c *Client) EnsureAIBotDebuggingAllowedGroups() error {
	val, err := c.GetSiteSetting("ai_bot_debugging_allowed_groups")
	if err != nil {
		return err
	}

	ids := parseInt64ListSetting(val)
	if len(ids) == 0 {
		ids = append(ids, discourseTrustLevel0GroupID)
	}
	for _, id := range ids {
		if id == discourseStaffGroupID {
			return nil
		}
	}

	ids = append(ids, discourseStaffGroupID)
	return c.SetSiteSetting("ai_bot_debugging_allowed_groups", formatInt64ListSetting(ids))
}

func (c *Client) AppendAIBotEnabledLLM(id int64) error {
	if id <= 0 {
		return nil
	}

	val, err := c.GetSiteSetting("ai_bot_enabled_llms")
	if err != nil {
		return err
	}

	ids := parseInt64ListSetting(val)
	for _, existing := range ids {
		if existing == id {
			return nil
		}
	}
	ids = append(ids, id)

	return c.SetSiteSetting("ai_bot_enabled_llms", formatInt64ListSetting(ids))
}
