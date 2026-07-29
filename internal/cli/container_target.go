package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/xdg"
)

// resolvedContainerTarget contains configuration-only container details. Resolving
// a target does not start a container, run hooks, or copy files.
type resolvedContainerTarget struct {
	configDir        string
	cfg              config.Config
	name             string
	image            config.ImageConfig
	effectiveWorkdir string
}

func resolveContainerTarget(cmd *cobra.Command, overrideName ...string) (resolvedContainerTarget, error) {
	configDir, err := xdg.ConfigDir()
	if err != nil {
		return resolvedContainerTarget{}, err
	}
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		return resolvedContainerTarget{}, err
	}

	name := ""
	if len(overrideName) > 0 {
		name = strings.TrimSpace(overrideName[0])
	}
	if name == "" {
		name, _ = cmd.Flags().GetString("name")
		name = strings.TrimSpace(name)
	}
	if name == "" {
		name = strings.TrimSpace(currentAgentName(cfg))
	}
	if name == "" {
		return resolvedContainerTarget{}, fmt.Errorf("no container selected; run 'dv start' first")
	}

	imageName := cfg.ContainerImages[name]
	_, image, err := resolveImage(cfg, imageName)
	if err != nil {
		return resolvedContainerTarget{}, fmt.Errorf("resolve image for container %q: %w", name, err)
	}

	return resolvedContainerTarget{
		configDir:        configDir,
		cfg:              cfg,
		name:             name,
		image:            image,
		effectiveWorkdir: config.EffectiveWorkdir(cfg, image, name),
	}, nil
}

func (target resolvedContainerTarget) discourseWorkdir() (string, error) {
	if target.image.Kind != "discourse" {
		return "", fmt.Errorf("command is only supported for discourse image kind; current: %q", target.image.Kind)
	}
	workdir := strings.TrimSpace(target.image.Workdir)
	if workdir == "" {
		workdir = "/var/www/discourse"
	}
	return workdir, nil
}
