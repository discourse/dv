package discourse

import (
	"context"

	"dv/internal/ai"
	"dv/internal/config"
	"dv/internal/docker"
)

// ClientWrapper wraps the HTTP client and implements DiscourseClient interface
type ClientWrapper struct {
	*Client
}

// NewClientWrapper creates a new Discourse client for the given container.
func NewClientWrapper(containerName string, cfg config.Config, envs docker.Envs, verbose bool) (*ClientWrapper, error) {
	httpClient, err := NewClient(containerName, cfg, envs, verbose)
	if err != nil {
		return nil, err
	}

	return &ClientWrapper{
		Client: httpClient,
	}, nil
}

// FetchState retrieves the LLM state
func (c *ClientWrapper) FetchState(ctx context.Context) (ai.LLMState, error) {
	if err := c.Client.EnsureAPIKey(); err != nil {
		return ai.LLMState{}, err
	}
	return c.Client.FetchState()
}

// CreateModel creates a new LLM model
func (c *ClientWrapper) CreateModel(ctx context.Context, input CreateLLMInput) (int64, error) {
	if err := c.Client.EnsureAPIKey(); err != nil {
		return 0, err
	}
	return c.Client.CreateLLM(input)
}

// UpdateModel updates an existing LLM model
func (c *ClientWrapper) UpdateModel(ctx context.Context, id int64, input CreateLLMInput) error {
	if err := c.Client.EnsureAPIKey(); err != nil {
		return err
	}
	return c.Client.UpdateLLM(id, input)
}

// DeleteModel removes an LLM model
func (c *ClientWrapper) DeleteModel(ctx context.Context, id int64) error {
	if err := c.Client.EnsureAPIKey(); err != nil {
		return err
	}
	return c.Client.DeleteLLM(id)
}

// TestModel tests an LLM configuration
func (c *ClientWrapper) TestModel(ctx context.Context, input CreateLLMInput) error {
	if err := c.Client.EnsureAPIKey(); err != nil {
		return err
	}
	return c.Client.TestLLM(input)
}

// SetDefaultLLM sets the default LLM
func (c *ClientWrapper) SetDefaultLLM(ctx context.Context, id int64) error {
	if err := c.Client.EnsureAPIKey(); err != nil {
		return err
	}
	return c.Client.SetDefaultLLM(id)
}

// EnableFeatures enables AI feature settings
func (c *ClientWrapper) EnableFeatures(ctx context.Context, settings []string, env map[string]string) error {
	if err := c.Client.EnsureAPIKey(); err != nil {
		return err
	}
	if err := c.Client.EnableSiteSettings(settings); err != nil {
		return err
	}
	// Also set GH_TOKEN if available
	if ghToken, ok := env["GH_TOKEN"]; ok && ghToken != "" {
		c.Client.SetSiteSetting("ai_bot_github_access_token", ghToken)
	}
	// Default debugging groups to trust_level_0 if unset.
	if err := c.Client.EnsureAIBotDebuggingAllowedGroupsDefault(); err != nil {
		c.Client.verboseLog("Warning: failed to ensure ai_bot_debugging_allowed_groups: %v", err)
	}
	return nil
}

// CreateAiSecret creates a named credential secret
func (c *ClientWrapper) CreateAiSecret(ctx context.Context, name, secret string) (int64, error) {
	if err := c.Client.EnsureAPIKey(); err != nil {
		return 0, err
	}
	return c.Client.CreateAiSecret(name, secret)
}

// UpdateAiSecret replaces the value of an existing credential secret
func (c *ClientWrapper) UpdateAiSecret(ctx context.Context, id int64, secret string) error {
	if err := c.Client.EnsureAPIKey(); err != nil {
		return err
	}
	return c.Client.UpdateAiSecret(id, secret)
}

// Ensure ClientWrapper implements DiscourseClient
var _ DiscourseClient = (*ClientWrapper)(nil)
