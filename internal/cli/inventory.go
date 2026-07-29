package cli

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"dv/internal/config"
	"dv/internal/localproxy"
)

// containerInventoryRecord is the shared, typed representation of a dv-owned
// container. Presentation layers (CLI, HTTP, and TUI) should format these
// records rather than rediscovering containers independently.
type containerInventoryRecord struct {
	name      string
	imageName string
	imageTag  string
	status    string
	time      string
	createdAt time.Time
	urls      []string
	selected  bool
}

type containerInventoryQuery func(context.Context) (string, error)

func dockerContainerInventoryQuery(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a", "--format", "{{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}\t{{.Labels}}\t{{.CreatedAt}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			return "", fmt.Errorf("docker ps: %w: %s", err, detail)
		}
		return "", fmt.Errorf("docker ps: %w", err)
	}
	return string(out), nil
}

func collectContainerInventory(ctx context.Context, cfg config.Config, imageName string, imgCfg config.ImageConfig, selected string, query containerInventoryQuery) ([]containerInventoryRecord, error) {
	if query == nil {
		query = dockerContainerInventoryQuery
	}
	out, err := query(ctx)
	if err != nil {
		return nil, err
	}

	proxyActive := cfg.LocalProxy.Enabled && localproxy.Running(cfg.LocalProxy)
	records := make([]containerInventoryRecord, 0)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 6)
		if len(parts) < 3 {
			continue
		}

		name, image, rawStatus := parts[0], parts[1], parts[2]
		portsField, labelsField, createdField := "", "", ""
		if len(parts) > 3 {
			portsField = parts[3]
		}
		if len(parts) > 4 {
			labelsField = parts[4]
		}
		if len(parts) > 5 {
			createdField = parts[5]
		}

		labels := parseLabels(labelsField)
		for key, value := range cfg.LabelOverrides[name] {
			labels[key] = value
		}
		if !containerBelongsToImage(cfg, name, image, labels, imageName, imgCfg.Tag) {
			continue
		}

		status, age := parseStatus(rawStatus)
		urls := parseHostPortURLs(portsField)
		if proxyActive {
			if host, _, _, httpPort, ok := localproxy.RouteFromLabels(labels); ok && host != "" {
				lp := cfg.LocalProxy
				lp.ApplyDefaults()
				if lp.HTTPS {
					if lp.HTTPSPort > 0 && lp.HTTPSPort != 443 {
						urls = []string{fmt.Sprintf("https://%s:%d", host, lp.HTTPSPort)}
					} else {
						urls = []string{"https://" + host}
					}
				} else {
					if httpPort <= 0 {
						httpPort = lp.HTTPPort
					}
					if httpPort > 0 && httpPort != 80 {
						urls = []string{fmt.Sprintf("http://%s:%d", host, httpPort)}
					} else {
						urls = []string{"http://" + host}
					}
				}
			}
		}

		records = append(records, containerInventoryRecord{
			name:      name,
			imageName: imageName,
			imageTag:  image,
			status:    status,
			time:      age,
			createdAt: parseDockerTime(createdField),
			urls:      urls,
			selected:  selected != "" && selected == name,
		})
	}

	sortInventoryRecords(records)
	return records, nil
}

func containerBelongsToImage(cfg config.Config, name, image string, labels map[string]string, imageName, imageTag string) bool {
	if mapped, ok := cfg.ContainerImages[name]; ok {
		return mapped == imageName
	}
	if labels["com.dv.owner"] == "dv" {
		return labels["com.dv.image-name"] == imageName
	}
	// Legacy containers predate ownership labels and provenance mappings.
	return image == imageTag
}

func sortInventoryRecords(records []containerInventoryRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		iRunning := records[i].status == "Running"
		jRunning := records[j].status == "Running"
		if iRunning != jRunning {
			return !iRunning && jRunning
		}
		iHasTime := !records[i].createdAt.IsZero()
		jHasTime := !records[j].createdAt.IsZero()
		if iHasTime && jHasTime {
			if records[i].createdAt.Equal(records[j].createdAt) {
				return records[i].name < records[j].name
			}
			return records[i].createdAt.Before(records[j].createdAt)
		}
		if iHasTime != jHasTime {
			return iHasTime
		}
		return records[i].name < records[j].name
	})
}
