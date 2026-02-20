package discourse

import (
	"context"

	"dv/internal/ai"
)

// DiscourseClient defines the interface for Discourse LLM operations.
type DiscourseClient interface {
	FetchState(ctx context.Context) (ai.LLMState, error)
	CreateModel(ctx context.Context, input CreateLLMInput) (int64, error)
	UpdateModel(ctx context.Context, id int64, input CreateLLMInput) error
	DeleteModel(ctx context.Context, id int64) error
	TestModel(ctx context.Context, input CreateLLMInput) error
	SetDefaultLLM(ctx context.Context, id int64) error
	EnableFeatures(ctx context.Context, settings []string, env map[string]string) error
	CreateAiSecret(ctx context.Context, name, secret string) (int64, error)
	UpdateAiSecret(ctx context.Context, id int64, secret string) error
}
