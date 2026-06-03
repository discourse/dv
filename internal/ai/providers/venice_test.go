package providers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestVeniceConnectorFetch_ParsesTextModels(t *testing.T) {
	t.Parallel()

	var gotAuth, gotPath string
	client := &http.Client{Transport: stubTransport{fn: func(r *http.Request) *http.Response {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.RequestURI()
		body := `{
  "data": [
    {
      "created": 1727966436,
      "id": "venice-uncensored",
      "model_spec": {
        "availableContextTokens": 131072,
        "capabilities": {
          "supportsFunctionCalling": true,
          "supportsReasoning": true,
          "supportsVision": true,
          "supportsWebSearch": true
        },
        "description": "Venice default uncensored model.",
        "name": "Venice Uncensored",
        "privacy": "private",
        "pricing": {
          "input": { "usd": 0.50, "diem": 0.50 },
          "output": { "usd": 2.00, "diem": 2.00 }
        },
        "traits": ["uncensored", "default"]
      },
      "object": "model",
      "owned_by": "venice.ai",
      "type": "text"
    },
    {
      "id": "flux-dev",
      "type": "image",
      "model_spec": { "name": "Flux Dev" }
    }
  ],
  "type": "text"
}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     http.StatusText(http.StatusOK),
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}
	}}}

	conn := &veniceConnector{}
	models, _, err := conn.fetch(context.Background(), client, map[string]string{
		"VENICE_API_KEY": " bearer test-key\n",
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("expected normalized Authorization header, got %q", gotAuth)
	}
	if gotPath != "/api/v1/models?type=all" {
		t.Fatalf("expected all models request, got %q", gotPath)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 text model, got %d", len(models))
	}

	model := models[0]
	if model.ID != "venice-uncensored" {
		t.Fatalf("ID = %q", model.ID)
	}
	if model.DisplayName != "Venice Uncensored" {
		t.Fatalf("DisplayName = %q", model.DisplayName)
	}
	if model.Provider != "open_ai" {
		t.Fatalf("Provider = %q, want open_ai", model.Provider)
	}
	if model.Endpoint != veniceAPIBaseURL+"/chat/completions" {
		t.Fatalf("Endpoint = %q", model.Endpoint)
	}
	if model.Tokenizer != "DiscourseAi::Tokenizer::OpenAiTokenizer" {
		t.Fatalf("Tokenizer = %q", model.Tokenizer)
	}
	if model.ContextTokens != 131072 {
		t.Fatalf("ContextTokens = %d", model.ContextTokens)
	}
	if model.InputCost != 0.50 || model.OutputCost != 2.00 {
		t.Fatalf("pricing = %f/%f", model.InputCost, model.OutputCost)
	}
	if !model.SupportsVision || !model.SupportsReasoning {
		t.Fatalf("expected vision and reasoning support")
	}
}

func TestVeniceConnectorFetch_Unauthorized(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: stubTransport{fn: func(r *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     http.StatusText(http.StatusUnauthorized),
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"bad key"}`)),
			Request:    r,
		}
	}}}

	conn := &veniceConnector{}
	_, _, err := conn.fetch(context.Background(), client, map[string]string{"VENICE_API_KEY": "bad-key"})
	if err == nil {
		t.Fatal("expected unauthorized error")
	}
	if !strings.Contains(err.Error(), "Venice AI authentication failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}
