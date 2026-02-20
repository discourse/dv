package providers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dv/internal/ai"
)

// Bedrock pricing for Claude models (approximate, may vary by region)
// Prices are in USD per 1M tokens
type bedrockPricing struct {
	InputCost       float64
	CachedInputCost float64
	OutputCost      float64
	ContextTokens   int
}

var bedrockModelPricing = map[string]bedrockPricing{
	"us.anthropic.claude-opus-4-6-v1": {
		InputCost:       5.0,
		CachedInputCost: 0.50,
		OutputCost:      25.0,
		ContextTokens:   200000,
	},
	"us.anthropic.claude-sonnet-4-6": {
		InputCost:       3.0,
		CachedInputCost: 0.30,
		OutputCost:      15.0,
		ContextTokens:   200000,
	},
	"us.anthropic.claude-sonnet-4-5-20250929-v1:0": {
		InputCost:       3.0,
		CachedInputCost: 0.30,
		OutputCost:      15.0,
		ContextTokens:   200000,
	},
	"us.anthropic.claude-haiku-4-5-20251001-v1:0": {
		InputCost:       1.0,
		CachedInputCost: 0.10,
		OutputCost:      5.0,
		ContextTokens:   200000,
	},
	// Also support without version suffix
	"us.anthropic.claude-sonnet-4-5-20250929-v1": {
		InputCost:       3.0,
		CachedInputCost: 0.30,
		OutputCost:      15.0,
		ContextTokens:   200000,
	},
	"us.anthropic.claude-haiku-4-5-20251001-v1": {
		InputCost:       1.0,
		CachedInputCost: 0.10,
		OutputCost:      5.0,
		ContextTokens:   200000,
	},
}

type bedrockConnector struct{}

func (c *bedrockConnector) id() string    { return "bedrock" }
func (c *bedrockConnector) title() string { return "AWS Bedrock" }
func (c *bedrockConnector) envKeys() []string {
	return []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}
}
func (c *bedrockConnector) hasCredentials(env map[string]string) bool {
	accessKey := envValue(env, "AWS_ACCESS_KEY_ID")
	secretKey := envValue(env, "AWS_SECRET_ACCESS_KEY")
	return accessKey != "" && secretKey != ""
}

func (c *bedrockConnector) fetch(ctx context.Context, client *http.Client, env map[string]string) ([]ai.ProviderModel, time.Time, error) {
	accessKey := envValue(env, "AWS_ACCESS_KEY_ID")
	secretKey := envValue(env, "AWS_SECRET_ACCESS_KEY")
	if accessKey == "" || secretKey == "" {
		return nil, time.Time{}, errMissingAPIKey
	}

	// Get region, default to us-west-2
	region := envValue(env, "AWS_REGION")
	if region == "" {
		region = "us-west-2"
	}

	now := time.Now()
	models := []ai.ProviderModel{}

	// Return hardcoded models
	modelIDs := []string{
		"us.anthropic.claude-opus-4-6-v1",
		"us.anthropic.claude-sonnet-4-6",
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"us.anthropic.claude-haiku-4-5-20251001-v1:0",
	}

	for _, modelID := range modelIDs {
		pricing := lookupBedrockPricing(modelID)
		displayName := formatBedrockModelName(modelID)

		// Bedrock endpoint format for Discourse
		// Discourse typically expects a specific endpoint format for Bedrock
		endpoint := fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", region)

		models = append(models, ai.ProviderModel{
			ID:                modelID,
			DisplayName:       displayName,
			Provider:          "aws_bedrock",
			Family:            "claude",
			Endpoint:          endpoint,
			Tokenizer:         "DiscourseAi::Tokenizer::AnthropicTokenizer",
			ContextTokens:     pricing.ContextTokens,
			InputCost:         pricing.InputCost,
			CachedInputCost:   pricing.CachedInputCost,
			OutputCost:        pricing.OutputCost,
			SupportsVision:    true,
			SupportsReasoning: false,
			Description:       fmt.Sprintf("%s via AWS Bedrock (%s)", displayName, region),
			Tags:              []string{"bedrock", "anthropic", "claude", "aws"},
			UpdatedAt:         now,
			Raw: map[string]interface{}{
				"model_id": modelID,
				"region":   region,
			},
		})
	}

	return models, now, nil
}

func lookupBedrockPricing(modelID string) bedrockPricing {
	// Try exact match first
	if pricing, ok := bedrockModelPricing[modelID]; ok {
		return pricing
	}

	// Try without version suffix
	baseID := strings.TrimSuffix(modelID, ":0")
	if pricing, ok := bedrockModelPricing[baseID]; ok {
		return pricing
	}

	// Try fuzzy matching
	lower := strings.ToLower(modelID)
	for key, pricing := range bedrockModelPricing {
		if strings.Contains(lower, strings.ToLower(key)) {
			return pricing
		}
	}

	// Default fallback (use Sonnet pricing)
	return bedrockPricing{
		InputCost:       3.0,
		CachedInputCost: 0.30,
		OutputCost:      15.0,
		ContextTokens:   200000,
	}
}

func formatBedrockModelName(modelID string) string {
	// Convert "us.anthropic.claude-sonnet-4-5-20250929-v1:0" to "Claude Sonnet 4.5"
	parts := strings.Split(modelID, ".")
	if len(parts) < 3 {
		return modelID
	}

	modelPart := parts[len(parts)-1]
	// Remove version suffix like ":0" or "-v1:0"
	modelPart = strings.Split(modelPart, ":")[0]
	modelPart = strings.TrimSuffix(modelPart, "-v1")

	// Split by dashes
	nameParts := strings.Split(modelPart, "-")
	if len(nameParts) < 3 {
		return modelID
	}

	// Extract model name and version
	// Format: "claude-sonnet-4-5-20250929" -> "Claude Sonnet 4.5"
	var result []string
	if len(nameParts) >= 1 && nameParts[0] == "claude" {
		result = append(result, "Claude")
		if len(nameParts) >= 2 {
			// Model name (sonnet or haiku)
			modelName := strings.Title(nameParts[1])
			result = append(result, modelName)
			// Version (4-5)
			if len(nameParts) >= 4 {
				version := fmt.Sprintf("%s.%s", nameParts[2], nameParts[3])
				result = append(result, version)
			}
		}
	}

	if len(result) == 0 {
		return modelID
	}

	return strings.Join(result, " ")
}
