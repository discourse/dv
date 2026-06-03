package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dv/internal/ai"
)

type connector interface {
	id() string
	title() string
	envKeys() []string
	hasCredentials(env map[string]string) bool
	fetch(ctx context.Context, client *http.Client, env map[string]string) ([]ai.ProviderModel, time.Time, error)
}

var builtinConnectors = []connector{
	&openRouterConnector{},
	&veniceConnector{},
	&openAIConnector{},
	&anthropicConnector{},
	&geminiConnector{},
	&bedrockConnector{},
}

func envValue(env map[string]string, key string) string {
	if env == nil {
		return ""
	}
	return strings.TrimSpace(env[key])
}

func firstEnv(env map[string]string, keys []string) string {
	for _, k := range keys {
		if v := envValue(env, k); v != "" {
			return v
		}
	}
	return ""
}

// generic error used when an API key is missing at runtime.
var errMissingAPIKey = errors.New("missing API key")

func unauthorizedErr(provider string) error {
	return fmt.Errorf("%s authentication failed (check API key)", provider)
}
