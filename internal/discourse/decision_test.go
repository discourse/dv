package discourse

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"dv/internal/ai"
)

// Exercise the HTTP helpers directly, as wrapper API-key setup requires Docker.
func decisionModelTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Api-Key") != "test-key" || r.Header.Get("Api-Username") != "system" {
			t.Error("missing API authentication headers")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing JSON content type")
		}
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client := NewClientWithURL("test", server.URL, "", nil, false)
	client.APIKey = "test-key"
	client.APIUsername = "system"
	return client
}

func TestFetchDecisionModels(t *testing.T) {
	client := decisionModelTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/admin/plugins/discourse-ai/ai-decision-models.json" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if _, err := w.Write([]byte(`{
			"ai_decision_models": [{"id":42,"display_name":"TypeSafe","name":"jev-latest","provider":"typesafe","url":"https://api.typesafe.ai/v1/systemone","ai_secret_id":7,"vision_enabled":true,"custom_headers":[{"name":"X-Model","value":"vision"}]}, {"id":-1,"display_name":"Seeded","ai_secret_id":null,"custom_headers":[]}],
			"meta":{"ai_secrets":[{"id":7,"name":"TypeSafe key"}],"providers":["typesafe"]}
		}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	})
	state, err := client.fetchDecisionModels()
	if err != nil {
		t.Fatalf("fetchDecisionModels: %v", err)
	}
	want := DecisionModelState{
		Models: []DecisionModel{
			{
				ID: 42, DisplayName: "TypeSafe", Name: "jev-latest", Provider: "typesafe",
				URL: "https://api.typesafe.ai/v1/systemone", AiSecretID: 7, VisionEnabled: true,
				CustomHeaders: []DecisionModelHeader{{Name: "X-Model", Value: "vision"}},
			},
			{ID: -1, DisplayName: "Seeded", CustomHeaders: []DecisionModelHeader{}},
		},
		AiSecrets: []ai.AiSecret{{ID: 7, Name: "TypeSafe key"}},
	}
	if !reflect.DeepEqual(state, want) {
		t.Errorf("state = %#v, want %#v", state, want)
	}
}

func TestSaveDecisionModel(t *testing.T) {
	for _, tt := range []struct {
		name   string
		id     int64
		method string
		path   string
		status int
	}{
		{"create", 0, http.MethodPost, "/admin/plugins/discourse-ai/ai-decision-models.json", http.StatusCreated},
		{"update", 42, http.MethodPut, "/admin/plugins/discourse-ai/ai-decision-models/42.json", http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := DecisionModel{
				ID: 99, DisplayName: "TypeSafe", Name: "jev-latest", Provider: "typesafe",
				URL: "https://api.typesafe.ai/v1/systemone", AiSecretID: 7, VisionEnabled: true,
				CustomHeaders: []DecisionModelHeader{{Name: "X-Model", Value: "vision"}},
			}
			client := decisionModelTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tt.method || r.URL.Path != tt.path {
					t.Errorf("request = %s %s, want %s %s", r.Method, r.URL.Path, tt.method, tt.path)
				}
				assertDecisionModelPayload(t, r, `{"ai_decision_model":{"display_name":"TypeSafe","name":"jev-latest","provider":"typesafe","url":"https://api.typesafe.ai/v1/systemone","ai_secret_id":7,"vision_enabled":true,"custom_headers":[{"name":"X-Model","value":"vision"}]}}`)
				w.WriteHeader(tt.status)
				if _, err := w.Write([]byte(`{"ai_decision_model":{"id":42}}`)); err != nil {
					t.Errorf("write response: %v", err)
				}
			})
			id, err := client.saveDecisionModel(tt.id, input)
			if err != nil {
				t.Fatalf("saveDecisionModel: %v", err)
			}
			if id != 42 {
				t.Errorf("id = %d, want 42", id)
			}
		})
	}
}

func TestDecisionModelEmptyHeaders(t *testing.T) {
	for _, operation := range []string{"save", "test"} {
		t.Run(operation, func(t *testing.T) {
			client := decisionModelTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				assertDecisionModelPayload(t, r, `{"ai_decision_model":{"display_name":"","name":"","provider":"","url":"","ai_secret_id":null,"vision_enabled":false,"custom_headers":[]}}`)
				if _, err := w.Write([]byte(`{"ai_decision_model":{"id":42},"success":true}`)); err != nil {
					t.Errorf("write response: %v", err)
				}
			})
			var err error
			if operation == "save" {
				_, err = client.saveDecisionModel(42, DecisionModel{})
			} else {
				err = client.testDecisionModel(DecisionModel{})
			}
			if err != nil {
				t.Fatalf("%s: %v", operation, err)
			}
		})
	}
}

func TestTestDecisionModel(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"success", 200, `{"success":true,"probability":0.99}`, ""},
		{"rejected", 200, `{"success":false,"error":"endpoint refused request"}`, "endpoint refused request"},
		{"false without error", 200, `{"success":false}`, "did not report success"},
		{"missing success", 200, `{}`, "did not report success"},
		{"null success", 200, `{"success":null}`, "did not report success"},
		{"invalid success", 200, `{"success":"true"}`, "decode test decision model response"},
		{"invalid JSON", 200, `{`, "decode test decision model response"},
		{"HTTP error", 429, `{"error":"rate limited"}`, "status 429"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := decisionModelTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/admin/plugins/discourse-ai/ai-decision-models/test.json" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				assertDecisionModelPayload(t, r, `{"ai_decision_model":{"display_name":"Test","name":"jev-latest","provider":"typesafe","url":"https://example.com","ai_secret_id":7,"vision_enabled":true,"custom_headers":[{"name":"X-Test","value":"yes"}]}}`)
				w.WriteHeader(tt.status)
				if _, err := w.Write([]byte(tt.body)); err != nil {
					t.Errorf("write response: %v", err)
				}
			})
			err := client.testDecisionModel(DecisionModel{
				DisplayName: "Test", Name: "jev-latest", Provider: "typesafe", URL: "https://example.com",
				AiSecretID: 7, VisionEnabled: true, CustomHeaders: []DecisionModelHeader{{Name: "X-Test", Value: "yes"}},
			})
			assertDecisionModelError(t, err, tt.wantErr)
		})
	}
}

func TestDecisionModelResponseErrors(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"HTTP error", 422, `{"errors":["invalid endpoint"]}`, "status 422"},
		{"invalid JSON", 200, `{`, "decode"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := decisionModelTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				if _, err := w.Write([]byte(tt.body)); err != nil {
					t.Errorf("write response: %v", err)
				}
			})
			_, err := client.fetchDecisionModels()
			assertDecisionModelError(t, err, tt.wantErr)
			_, err = client.saveDecisionModel(0, DecisionModel{})
			assertDecisionModelError(t, err, tt.wantErr)
			if tt.status == 422 && (err == nil || !strings.Contains(err.Error(), "invalid endpoint")) {
				t.Errorf("save error should include response body: %v", err)
			}
		})
	}
}

func TestSaveDecisionModelMissingID(t *testing.T) {
	client := decisionModelTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`{"ai_decision_model":{}}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	})
	_, err := client.saveDecisionModel(0, DecisionModel{})
	assertDecisionModelError(t, err, "missing model ID")
}

func assertDecisionModelPayload(t *testing.T, r *http.Request, wantJSON string) {
	t.Helper()
	var got, want interface{}
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Errorf("decode request: %v", err)
		return
	}
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("decode expected payload: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("payload = %#v, want %#v", got, want)
	}
}

func assertDecisionModelError(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	} else if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want containing %q", err, want)
	}
}
