package cli

import (
	"strings"
	"testing"

	"dv/internal/config"
)

func TestBuildConsoleScriptIsReadyAndPreservesPryConfig(t *testing.T) {
	t.Parallel()

	script := buildConsoleScript("hades")
	ready := strings.Index(script, "pg_isready")
	console := strings.Index(script, "bin/rails console")
	if ready == -1 || console == -1 || ready > console {
		t.Fatalf("PostgreSQL readiness must precede Rails console:\n%s", script)
	}
	for _, want := range []string{
		"mktemp /tmp/dv-pryrc.XXXXXX",
		`trap 'rm -f "$pryrc"' EXIT`,
		`ENV["XDG_CONFIG_HOME"]`,
		`File.join(xdg_config_home, "pry", "pryrc")`,
		`File.expand_path("~/.pryrc")`,
		`load user_pryrc if user_pryrc`,
		`Pry.config.prompt_name = `,
		`dv:hades`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("console script missing %q:\n%s", want, script)
		}
	}
}

func TestRubySingleQuotedEscapesWithoutInterpolation(t *testing.T) {
	t.Parallel()

	got := rubySingleQuoted(`agent#{danger}'\name`)
	want := `'agent#{danger}\'\\name'`
	if got != want {
		t.Fatalf("ruby literal = %q, want %q", got, want)
	}
}

func TestResolvedContainerTargetDiscourseWorkdir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		image   config.ImageConfig
		want    string
		wantErr bool
	}{
		{name: "configured", image: config.ImageConfig{Kind: "discourse", Workdir: "/srv/discourse"}, want: "/srv/discourse"},
		{name: "default", image: config.ImageConfig{Kind: "discourse"}, want: "/var/www/discourse"},
		{name: "custom image", image: config.ImageConfig{Kind: "custom", Workdir: "/workspace"}, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := (resolvedContainerTarget{
				image:            test.image,
				effectiveWorkdir: "/custom/workspace-that-is-not-rails",
			}).discourseWorkdir()
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected error, got workdir %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("got workdir %q, want %q", got, test.want)
			}
		})
	}
}
