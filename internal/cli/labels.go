package cli

import (
	"dv/internal/config"
	"dv/internal/docker"
)

func labelsWithOverrides(name string, cfg config.Config) (map[string]string, error) {
	labels, err := docker.Labels(name)
	if err != nil {
		// If the container is gone but we have overrides, use those alone.
		if len(cfg.LabelOverrides[name]) > 0 {
			labels = make(map[string]string, len(cfg.LabelOverrides[name]))
		} else {
			return nil, err
		}
	}
	for k, v := range cfg.LabelOverrides[name] {
		labels[k] = v
	}
	return labels, nil
}
