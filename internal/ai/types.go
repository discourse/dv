package ai

import (
	"encoding/json"
	"time"
)

// LLMQuota represents a per-group quota definition.
type LLMQuota struct {
	GroupID         int  `json:"group_id"`
	MaxTokens       *int `json:"max_tokens"`
	MaxUsages       *int `json:"max_usages"`
	DurationSeconds int  `json:"duration_seconds"`
}

// LLMModel describes a language model stored inside Discourse.
type LLMModel struct {
	ID               int64                    `json:"id"`
	DisplayName      string                   `json:"display_name"`
	Name             string                   `json:"name"`
	Provider         string                   `json:"provider"`
	Tokenizer        string                   `json:"tokenizer"`
	URL              string                   `json:"url"`
	MaxPromptTokens  int                      `json:"max_prompt_tokens"`
	MaxOutputTokens  int                      `json:"max_output_tokens"`
	InputCost        float64                  `json:"input_cost"`
	CachedInputCost  float64                  `json:"cached_input_cost"`
	OutputCost       float64                  `json:"output_cost"`
	EnabledChatBot   bool                     `json:"enabled_chat_bot"`
	VisionEnabled    bool                     `json:"vision_enabled"`
	AiSecretID       int64                    `json:"ai_secret_id"`
	ProviderParams   map[string]interface{}   `json:"provider_params"`
	UsedBy           []LLMUsage               `json:"used_by"`
	Quotas           []LLMQuota               `json:"llm_quotas"`
	CreditAllocation map[string]interface{}   `json:"llm_credit_allocation,omitempty"`
	FeatureCosts     []map[string]interface{} `json:"llm_feature_credit_costs,omitempty"`
}

type LLMUsage struct {
	Type string `json:"type"`
}

// TokenizerMeta describes an available tokenizer option.
type TokenizerMeta struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AiSecret represents a stored credential in the ai_secrets table.
type AiSecret struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// LLMMetadata includes supplemental information returned from Discourse.
type LLMMetadata struct {
	Tokenizers     []TokenizerMeta                   `json:"tokenizers"`
	Providers      []string                          `json:"providers"`
	ProviderParams map[string]map[string]interface{} `json:"provider_params"`
	Presets        json.RawMessage                   `json:"presets"`
	AiSecrets      []AiSecret                        `json:"ai_secrets"`
}

// LLMState is the aggregate payload consumed by the TUI.
type LLMState struct {
	Models    []LLMModel
	DefaultID int64
	Meta      LLMMetadata
}

// ProviderModel describes an available remote model.
type ProviderModel struct {
	ID                string                 `json:"id"`
	DisplayName       string                 `json:"display_name"`
	Provider          string                 `json:"provider"`
	Family            string                 `json:"family"`
	Endpoint          string                 `json:"endpoint"`
	Tokenizer         string                 `json:"tokenizer"`
	ContextTokens     int                    `json:"context_tokens"`
	InputCost         float64                `json:"input_cost"`
	CachedInputCost   float64                `json:"cached_input_cost"`
	OutputCost        float64                `json:"output_cost"`
	SupportsVision    bool                   `json:"supports_vision"`
	SupportsReasoning bool                   `json:"supports_reasoning"`
	Description       string                 `json:"description"`
	Tags              []string               `json:"tags"`
	UpdatedAt         time.Time              `json:"updated_at"`
	Raw               map[string]interface{} `json:"raw"`
}

// ProviderEntry aggregates models fetched from a single provider account/key.
type ProviderEntry struct {
	ID             string          `json:"id"`
	Title          string          `json:"title"`
	EnvKeys        []string        `json:"env_keys"`
	HasCredentials bool            `json:"has_credentials"`
	LastUpdated    time.Time       `json:"last_updated"`
	Error          string          `json:"error"`
	Models         []ProviderModel `json:"models"`
}

// ProviderCatalog is the combined output from all connectors.
type ProviderCatalog struct {
	Entries []ProviderEntry
}
