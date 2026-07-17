package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"dv/internal/localproxy"
)

func TestNewAgentRequestArgs(t *testing.T) {
	t.Parallel()

	req := newAgentRequest{
		Name:          "core-pr-123",
		Image:         "discourse",
		Template:      "/tmp/site.yml",
		KeepOnFailure: true,
		Verbose:       true,
		PR:            "123",
		Branch:        "feature/test",
		Plugins:       []string{"discourse-solved", "owner/plugin"},
		LocalPlugins:  []string{"/tmp/local-plugin"},
		Themes:        []string{"discourse-air"},
		WithoutTestDB: true,
	}

	want := []string{
		"new",
		"--image", "discourse",
		"--template", "/tmp/site.yml",
		"--keep-on-failure",
		"--verbose",
		"--pr", "123",
		"--branch", "feature/test",
		"--plugin", "discourse-solved",
		"--plugin", "owner/plugin",
		"--plugin-local", "/tmp/local-plugin",
		"--theme", "discourse-air",
		"--without-test-db",
		"core-pr-123",
	}
	if got := req.args(); !reflect.DeepEqual(got, want) {
		t.Fatalf("args() = %#v, want %#v", got, want)
	}
}

func TestNewAgentRequestArgsMinimal(t *testing.T) {
	t.Parallel()

	if got, want := (newAgentRequest{}).args(), []string{"new"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("args() = %#v, want %#v", got, want)
	}
}

func TestHandleContainerNewRejectsUnsupportedMethod(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/containers/new", nil)
	handleContainerNew(recorder, request, t.TempDir())

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleContainerNewRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/containers/new", bytes.NewBufferString(`{"unknown":true}`))
	handleContainerNew(recorder, request, t.TempDir())

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if !strings.Contains(recorder.Body.String(), "unknown field") {
		t.Fatalf("body = %q, want unknown-field error", recorder.Body.String())
	}
}

func TestHandleContainerNewRejectsUnsafeExplicitName(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/containers/new", bytes.NewBufferString(`{"name":"not_safe"}`))
	handleContainerNew(recorder, request, t.TempDir())

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if !strings.Contains(recorder.Body.String(), "numbers, letters, dashes, and dots") {
		t.Fatalf("body = %q, want hostname-safety error", recorder.Body.String())
	}
}

func TestContainerResultHostname(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name: "local proxy hostname",
			labels: map[string]string{
				localproxy.LabelHost:          "core-pr-41785.dv.localhost",
				localproxy.LabelTargetPort:    "3001",
				localproxy.LabelContainerPort: "3000",
			},
			want: "core-pr-41785.dv.localhost",
		},
		{
			name:   "no local proxy metadata",
			labels: map[string]string{},
			want:   "localhost",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := containerResultHostname(test.labels); got != test.want {
				t.Fatalf("containerResultHostname() = %q, want %q", got, test.want)
			}
		})
	}
}
