package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dv/internal/ai"
)

const veniceAPIBaseURL = "https://api.venice.ai/api/v1"

type veniceConnector struct{}

func (c *veniceConnector) id() string    { return "venice" }
func (c *veniceConnector) title() string { return "Venice AI" }
func (c *veniceConnector) envKeys() []string {
	return []string{"VENICE_API_KEY"}
}
func (c *veniceConnector) hasCredentials(env map[string]string) bool {
	return firstEnv(env, c.envKeys()) != ""
}

func (c *veniceConnector) fetch(ctx context.Context, client *http.Client, env map[string]string) ([]ai.ProviderModel, time.Time, error) {
	apiKey := firstEnv(env, c.envKeys())
	if apiKey == "" {
		return nil, time.Time{}, errMissingAPIKey
	}
	apiKey = normalizeVeniceAPIKey(apiKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, veniceAPIBaseURL+"/models?type=all", nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", "dv/ai-config")

	resp, err := client.Do(req)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, time.Time{}, unauthorizedErr("Venice AI")
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, time.Time{}, fmt.Errorf("venice ai %s: %s", resp.Status, string(body))
	}

	var root struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		return nil, time.Time{}, err
	}

	now := time.Now()
	models := make([]ai.ProviderModel, 0, len(root.Data))
	for _, raw := range root.Data {
		var obj map[string]interface{}
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}
		modelType := strings.ToLower(firstStringValue(obj, "type"))
		if modelType != "" && modelType != "text" {
			continue
		}

		id := firstStringValue(obj, "id")
		if id == "" {
			continue
		}

		spec, _ := obj["model_spec"].(map[string]interface{})
		displayName := firstStringValue(spec, "name")
		if displayName == "" {
			displayName = firstStringValue(obj, "name")
		}
		if displayName == "" {
			displayName = id
		}

		description := firstStringValue(spec, "description")
		if description == "" {
			description = firstStringValue(obj, "description")
		}

		contextTokens := int(floatValue(obj["context_length"]))
		if contextTokens <= 0 && spec != nil {
			contextTokens = int(floatValue(spec["availableContextTokens"]))
		}

		var inputCost, outputCost, cachedCost float64
		if spec != nil {
			if pricing, ok := spec["pricing"].(map[string]interface{}); ok {
				inputCost = priceFromValue(pricing["input"])
				outputCost = priceFromValue(pricing["output"])
				cachedCost = priceFromValue(pricing["cached"])
			}
		}
		// Venice model_spec.pricing is already USD per 1M tokens. If the API ever
		// returns OpenAI/OpenRouter-style per-token pricing at the top level, convert it.
		if inputCost == 0 && outputCost == 0 {
			if pricing, ok := obj["pricing"].(map[string]interface{}); ok {
				inputCost = priceFromValue(pricing["prompt"])
				if inputCost == 0 {
					inputCost = priceFromValue(pricing["input"])
				}
				outputCost = priceFromValue(pricing["completion"])
				if outputCost == 0 {
					outputCost = priceFromValue(pricing["output"])
				}
				cachedCost = priceFromValue(pricing["cached_prompt"])
				if cachedCost == 0 {
					cachedCost = priceFromValue(pricing["cached"])
				}
				inputCost *= 1_000_000
				outputCost *= 1_000_000
				cachedCost *= 1_000_000
			}
		}

		var tags []string
		if privacy := firstStringValue(spec, "privacy"); privacy != "" {
			tags = append(tags, privacy)
		}
		if rawTraits, ok := spec["traits"].([]interface{}); ok {
			for _, t := range rawTraits {
				if v := stringValue(t); v != "" {
					tags = append(tags, v)
				}
			}
		}

		caps, _ := spec["capabilities"].(map[string]interface{})
		supportsVision := boolValue(caps["supportsVision"])
		supportsReasoning := boolValue(caps["supportsReasoning"]) || boolValue(caps["supportsReasoningEffort"])
		if boolValue(caps["supportsFunctionCalling"]) {
			tags = append(tags, "tools")
		}
		if boolValue(caps["supportsWebSearch"]) {
			tags = append(tags, "web-search")
		}
		if supportsReasoning {
			tags = append(tags, "reasoning")
		}
		if supportsVision {
			tags = append(tags, "vision")
		}

		models = append(models, ai.ProviderModel{
			ID:                id,
			DisplayName:       displayName,
			Provider:          "open_ai",
			Family:            "venice",
			Endpoint:          veniceAPIBaseURL + "/chat/completions",
			Tokenizer:         "DiscourseAi::Tokenizer::OpenAiTokenizer",
			ContextTokens:     contextTokens,
			InputCost:         inputCost,
			CachedInputCost:   cachedCost,
			OutputCost:        outputCost,
			SupportsVision:    supportsVision,
			SupportsReasoning: supportsReasoning,
			Description:       description,
			Tags:              tags,
			UpdatedAt:         now,
			Raw:               obj,
		})
	}

	return models, now, nil
}

func normalizeVeniceAPIKey(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if len(apiKey) >= len("Bearer ") && strings.EqualFold(apiKey[:len("Bearer ")], "Bearer ") {
		apiKey = strings.TrimSpace(apiKey[len("Bearer "):])
	}
	return apiKey
}

func firstStringValue(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	value, ok := m[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(stringValue(value))
}
