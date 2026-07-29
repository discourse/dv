package cli

import (
	"fmt"
	"io"

	"dv/internal/config"

	"gopkg.in/yaml.v3"
)

type sshForwardMode string

const (
	sshForwardOff          sshForwardMode = "off"
	sshForwardProvisioning sshForwardMode = "provisioning"
	sshForwardAlways       sshForwardMode = "always"
)

type sshForwardSetting struct {
	Mode sshForwardMode
	Set  bool
}

func (s *sshForwardSetting) UnmarshalYAML(node *yaml.Node) error {
	switch node.Tag {
	case "!!bool":
		var enabled bool
		if err := node.Decode(&enabled); err != nil {
			return err
		}
		if enabled {
			s.Mode = sshForwardAlways
		} else {
			s.Mode = sshForwardOff
		}
		s.Set = true
		return nil
	case "!!str":
		mode, err := parseSSHForwardMode(node.Value)
		if err != nil {
			return err
		}
		s.Mode = mode
		s.Set = true
		return nil
	default:
		return fmt.Errorf("ssh_forward must be one of off, provisioning, or always (booleans remain supported)")
	}
}

func parseSSHForwardMode(value string) (sshForwardMode, error) {
	mode := sshForwardMode(value)
	switch mode {
	case sshForwardOff, sshForwardProvisioning, sshForwardAlways:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid ssh_forward mode %q: must be one of off, provisioning, or always", value)
	}
}

func (s sshForwardSetting) effective() sshForwardMode {
	if !s.Set {
		return sshForwardOff
	}
	switch s.Mode {
	case sshForwardOff, sshForwardProvisioning, sshForwardAlways:
		return s.Mode
	default:
		return sshForwardOff
	}
}

func (m sshForwardMode) enabled() bool {
	return m == sshForwardProvisioning || m == sshForwardAlways
}

func resolveSSHForwardMode(flag string, setting sshForwardSetting, required bool) (sshForwardMode, error) {
	mode := setting.effective()
	if flag != "" {
		var err error
		mode, err = parseSSHForwardMode(flag)
		if err != nil {
			return "", err
		}
	} else if !setting.Set && required {
		mode = sshForwardProvisioning
	}
	if required && mode == sshForwardOff {
		return "", fmt.Errorf("SSH repository URLs require SSH forwarding; use ssh_forward: provisioning or --ssh-forward=provisioning")
	}
	return mode, nil
}

func parseTemplateConfig(r io.Reader) (*templateConfig, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	tpl := &templateConfig{}
	if err := decoder.Decode(tpl); err != nil {
		return nil, fmt.Errorf("parse template YAML: %w", err)
	}

	if !tpl.Git.SSHForward.Set {
		tpl.Git.SSHForward.Mode = sshForwardOff
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("parse template YAML: %w", err)
		}
		return nil, fmt.Errorf("parse template YAML: multiple YAML documents are not supported")
	}
	return tpl, nil
}

type templateConfig struct {
	Discourse struct {
		Branch string `yaml:"branch"`
		PR     int    `yaml:"pr"`
		Repo   string `yaml:"repo"`
	} `yaml:"discourse"`
	Git struct {
		SSHForward sshForwardSetting `yaml:"ssh_forward"`
	} `yaml:"git"`
	Copy     []config.CopyRule `yaml:"copy"`
	Env      map[string]string `yaml:"env"`
	OnCreate []string          `yaml:"on_create"`
	Plugins  []templatePlugin  `yaml:"plugins"`
	Themes   []templateTheme   `yaml:"themes"`
	Settings map[string]any    `yaml:"settings"`
	MCP      []templateMCP     `yaml:"mcp"`
	Mounts   []templateMount   `yaml:"mounts"`
}

type templateMount struct {
	Host      string `yaml:"host"`
	Container string `yaml:"container"`
	ReadOnly  bool   `yaml:"read_only"`
}

type templatePlugin struct {
	Repo   string `yaml:"repo"`
	Path   string `yaml:"path"`
	Branch string `yaml:"branch"`
}

type templateTheme struct {
	Repo      string `yaml:"repo"`
	Name      string `yaml:"name"`
	Path      string `yaml:"path"`
	Branch    string `yaml:"branch"`
	PR        int    `yaml:"pr"`
	Enabled   *bool  `yaml:"enabled"`
	AutoWatch bool   `yaml:"auto_watch"`
}

type templateMCP struct {
	Name    string   `yaml:"name"`
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
}
