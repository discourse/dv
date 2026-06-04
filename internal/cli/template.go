package cli

import (
	"dv/internal/config"
)

type templateConfig struct {
	Discourse struct {
		Branch string `yaml:"branch"`
		PR     int    `yaml:"pr"`
		Repo   string `yaml:"repo"`
	} `yaml:"discourse"`
	Git struct {
		SSHForward bool `yaml:"ssh_forward"`
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
