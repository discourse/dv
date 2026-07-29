package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTemplateSSHForwardModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		yaml string
		mode sshForwardMode
		set  bool
	}{
		{name: "omitted", yaml: "discourse:\n  branch: main\n", mode: sshForwardOff, set: false},
		{name: "off", yaml: "git:\n  ssh_forward: off\n", mode: sshForwardOff, set: true},
		{name: "provisioning", yaml: "git:\n  ssh_forward: provisioning\n", mode: sshForwardProvisioning, set: true},
		{name: "always", yaml: "git:\n  ssh_forward: always\n", mode: sshForwardAlways, set: true},
		{name: "legacy false", yaml: "git:\n  ssh_forward: false\n", mode: sshForwardOff, set: true},
		{name: "legacy true", yaml: "git:\n  ssh_forward: true\n", mode: sshForwardAlways, set: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tpl, err := parseTemplateConfig(strings.NewReader(tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if tpl.Git.SSHForward.Mode != tt.mode || tpl.Git.SSHForward.Set != tt.set {
				t.Fatalf("ssh_forward = %+v, want mode %q set %t", tpl.Git.SSHForward, tt.mode, tt.set)
			}
		})
	}
}

func TestResolveSSHForwardMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		flag     string
		setting  sshForwardSetting
		required bool
		want     sshForwardMode
		wantErr  bool
	}{
		{name: "defaults off", want: sshForwardOff},
		{name: "zero value fails closed", setting: sshForwardSetting{Set: true}, want: sshForwardOff},
		{name: "infers provisioning", required: true, want: sshForwardProvisioning},
		{name: "template always", setting: sshForwardSetting{Mode: sshForwardAlways, Set: true}, required: true, want: sshForwardAlways},
		{name: "template provisioning", setting: sshForwardSetting{Mode: sshForwardProvisioning, Set: true}, required: true, want: sshForwardProvisioning},
		{name: "explicit off conflicts", setting: sshForwardSetting{Mode: sshForwardOff, Set: true}, required: true, wantErr: true},
		{name: "flag overrides template", flag: "provisioning", setting: sshForwardSetting{Mode: sshForwardAlways, Set: true}, want: sshForwardProvisioning},
		{name: "flag off conflicts", flag: "off", setting: sshForwardSetting{Mode: sshForwardAlways, Set: true}, required: true, wantErr: true},
		{name: "invalid flag", flag: "sometimes", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveSSHForwardMode(tt.flag, tt.setting, tt.required)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveSSHForwardMode() error = %v, wantErr %t", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("resolveSSHForwardMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseTemplateRejectsInvalidSSHForwardMode(t *testing.T) {
	t.Parallel()

	_, err := parseTemplateConfig(strings.NewReader("git:\n  ssh_forward: sometimes\n"))
	if err == nil || !strings.Contains(err.Error(), `invalid ssh_forward mode "sometimes"`) {
		t.Fatalf("error = %v, want invalid mode error", err)
	}
}

func TestParseTemplateRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		"git:\n  ssh_foward: provisioning\n",
		"plugins:\n  - repo: https://example.com/plugin.git\n    brnach: main\n",
	} {
		_, err := parseTemplateConfig(strings.NewReader(input))
		if err == nil || !strings.Contains(err.Error(), "field") || !strings.Contains(err.Error(), "not found") {
			t.Errorf("parseTemplateConfig(%q) error = %v, want unknown field error", input, err)
		}
	}
}

func TestTrackedTemplatesParseStrictly(t *testing.T) {
	t.Parallel()

	paths, err := filepath.Glob("../../templates/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no tracked templates found")
	}
	for _, path := range paths {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := parseTemplateConfig(file); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParseTemplateRejectsMultipleDocuments(t *testing.T) {
	t.Parallel()

	_, err := parseTemplateConfig(strings.NewReader("discourse:\n  branch: main\n---\ndiscourse:\n  branch: stable\n"))
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("error = %v, want multiple document error", err)
	}
}
