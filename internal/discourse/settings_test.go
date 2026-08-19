package discourse

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnsureAIBotDebuggingAllowedGroups(t *testing.T) {
	tests := []struct {
		name      string
		current   interface{}
		wantValue string
		wantPut   bool
	}{
		{
			name:      "empty setting gets trust level zero and staff",
			current:   "",
			wantValue: "10|3",
			wantPut:   true,
		},
		{
			name:      "existing groups are preserved when staff is appended",
			current:   "42|10",
			wantValue: "42|10|3",
			wantPut:   true,
		},
		{
			name:    "staff is not duplicated",
			current: "42|3",
			wantPut: false,
		},
		{
			name:      "legacy trust level name is converted to group IDs",
			current:   "trust_level_0",
			wantValue: "10|3",
			wantPut:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			putCount := 0
			var gotValue interface{}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/admin/site_settings.json":
					if got := r.URL.Query().Get("filter"); got != "ai_bot_debugging_allowed_groups" {
						t.Errorf("filter = %q, want ai_bot_debugging_allowed_groups", got)
					}
					w.Header().Set("Content-Type", "application/json")
					if err := json.NewEncoder(w).Encode(SiteSettingsResponse{SiteSettings: []SiteSetting{{
						Setting: "ai_bot_debugging_allowed_groups",
						Value:   tt.current,
					}}}); err != nil {
						t.Errorf("encode GET response: %v", err)
					}
				case r.Method == http.MethodPut && r.URL.Path == "/admin/site_settings/ai_bot_debugging_allowed_groups.json":
					putCount++
					var payload map[string]interface{}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("decode PUT request: %v", err)
					}
					gotValue = payload["ai_bot_debugging_allowed_groups"]
					w.WriteHeader(http.StatusNoContent)
				default:
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			}))
			defer server.Close()

			client := NewClientWithURL("test", server.URL, "", nil, false)
			client.APIKey = "test-key"
			client.APIUsername = "system"
			if err := client.EnsureAIBotDebuggingAllowedGroups(); err != nil {
				t.Fatalf("EnsureAIBotDebuggingAllowedGroups returned error: %v", err)
			}

			if tt.wantPut {
				if putCount != 1 {
					t.Fatalf("PUT count = %d, want 1", putCount)
				}
				if gotValue != tt.wantValue {
					t.Errorf("setting value = %#v, want %q", gotValue, tt.wantValue)
				}
			} else if putCount != 0 {
				t.Errorf("PUT count = %d, want 0", putCount)
			}
		})
	}
}
