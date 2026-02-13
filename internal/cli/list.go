package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/localproxy"
	"dv/internal/xdg"
)

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List containers created from the selected image",
	RunE: func(cmd *cobra.Command, args []string) error {
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}

		imgName, imgCfg, err := resolveImage(cfg, "")
		if err != nil {
			return err
		}

		proxyActive := cfg.LocalProxy.Enabled && localproxy.Running(cfg.LocalProxy)

		// Include Ports, Labels, and CreatedAt for discovery, clickable URLs, and ordering
		out, _ := runShell("docker ps -a --format '{{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}\t{{.Labels}}\t{{.CreatedAt}}'")
		selected := currentAgentName(cfg)
		var agents []agentInfo

		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			parts := strings.SplitN(line, "\t", 6)
			if len(parts) < 3 {
				continue
			}
			name, image, status := parts[0], parts[1], parts[2]
			portsField := ""
			if len(parts) >= 4 {
				portsField = parts[3]
			}
			labelsField := ""
			if len(parts) >= 5 {
				labelsField = parts[4]
			}
			labelMap := parseLabels(labelsField)
			createdAt := time.Time{}
			if len(parts) >= 6 {
				createdAt = parseDockerTime(parts[5])
			}
			// Determine if this container belongs to the selected image
			belongs := false
			if imgNameFromCfg, ok := cfg.ContainerImages[name]; ok && imgNameFromCfg == imgName {
				belongs = true
			}
			if !belongs {
				if labelMap["com.dv.owner"] == "dv" && labelMap["com.dv.image-name"] == imgName {
					belongs = true
				}
			}
			if !belongs {
				// Legacy fallback: match by raw image tag
				if image == imgCfg.Tag {
					belongs = true
				}
			}
			if !belongs {
				continue
			}

			// Parse status and time
			statusText, timeText := parseStatus(status)
			urls := parseHostPortURLs(portsField)
			if proxyActive {
				if host, _, _, httpPort, ok := localproxy.RouteFromLabels(labelMap); ok && host != "" {
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

			agents = append(agents, agentInfo{
				name:      name,
				status:    statusText,
				time:      timeText,
				createdAt: createdAt,
				urls:      urls,
				selected:  selected != "" && name == selected,
			})
		}

		sortAgents(agents)

		withSessions, _ := cmd.Flags().GetBool("sessions")
		if withSessions {
			for i, agent := range agents {
				if agent.status == "Running" {
					s, err := docker.ExecSessions(agent.name)
					if err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not check sessions for '%s': %v\n", agent.name, err)
						agents[i].sessions = -1
					} else {
						agents[i].sessions = len(s)
					}
				}
			}
		}

		// Print in ls -l style format
		if len(agents) == 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "(no agents found for image '%s')\n", imgCfg.Tag)
		} else {
			// Calculate dynamic column width based on longest name
			maxNameWidth := calculateMaxNameWidth(agents)

			fmt.Fprintf(cmd.OutOrStdout(), "total %d\n", len(agents))
			for _, agent := range agents {
				mark := " "
				if agent.selected {
					mark = "*"
				}
				sessionSuffix := ""
				if agent.sessions < 0 {
					sessionSuffix = "  [sessions: ?]"
				} else if agent.sessions > 0 {
					sessionSuffix = fmt.Sprintf("  [%d session", agent.sessions)
					if agent.sessions != 1 {
						sessionSuffix += "s"
					}
					sessionSuffix += "]"
				}
				if len(agent.urls) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "%s %-*s %-8s %-12s %s%s\n",
						mark, maxNameWidth, agent.name, agent.status, agent.time, strings.Join(agent.urls, " "), sessionSuffix)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "%s %-*s %-8s %-12s%s\n",
						mark, maxNameWidth, agent.name, agent.status, agent.time, sessionSuffix)
				}
			}
		}

		if selected != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "\nSelected: %s\n", selected)
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "\nSelected: (none)")
		}
		_ = imgName // not printed but kept for clarity
		return nil
	},
}

func init() {
	listCmd.Flags().BoolP("sessions", "s", false, "Show active session counts (slower)")
}

// parseHostPortURLs extracts host ports from a Docker "Ports" column value and
// returns clickable http://localhost:<port> URLs.
// Examples of input formats handled:
//
//	"0.0.0.0:4201->4200/tcp, :::4201->4200/tcp"
//	"127.0.0.1:8080->8080/tcp"
//	"4200/tcp" (no published ports)
func parseHostPortURLs(portsField string) []string {
	portsField = strings.TrimSpace(portsField)
	if portsField == "" {
		return nil
	}
	// Multiple mappings separated by commas
	segments := strings.Split(portsField, ",")
	var urls []string
	seen := map[string]struct{}{}
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		// Look for the left side before "->" which contains host ip:port
		arrowIdx := strings.Index(seg, "->")
		if arrowIdx == -1 {
			// Not a published mapping (e.g., "4200/tcp")
			continue
		}
		left := strings.TrimSpace(seg[:arrowIdx])
		// left may be like "0.0.0.0:4201" or ":::4201" or "127.0.0.1:4201"
		colonIdx := strings.LastIndex(left, ":")
		if colonIdx == -1 || colonIdx+1 >= len(left) {
			continue
		}
		hostPort := left[colonIdx+1:]
		// Basic numeric validation
		if hostPort == "" {
			continue
		}
		url := "http://localhost:" + hostPort
		if _, ok := seen[url]; !ok {
			seen[url] = struct{}{}
			urls = append(urls, url)
		}
	}
	return urls
}

// parseLabels converts a docker --format {{.Labels}} string (comma-separated key=value pairs)
// into a map for easy lookup. Malformed entries are ignored.
func parseLabels(labelsField string) map[string]string {
	labelsField = strings.TrimSpace(labelsField)
	if labelsField == "" {
		return map[string]string{}
	}
	items := strings.Split(labelsField, ",")
	out := make(map[string]string, len(items))
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		kv := strings.SplitN(it, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		if key != "" {
			out[key] = val
		}
	}
	return out
}

// agentInfo holds information about a container for formatted display
type agentInfo struct {
	name      string
	status    string
	time      string
	createdAt time.Time
	urls      []string
	selected  bool
	sessions  int
}

// calculateMaxNameWidth finds the longest agent name and returns an appropriate column width
// with reasonable limits (minimum 10, maximum 50 characters)
func calculateMaxNameWidth(agents []agentInfo) int {
	maxWidth := 10 // minimum width

	for _, agent := range agents {
		if len(agent.name) > maxWidth {
			maxWidth = len(agent.name)
		}
	}

	// Apply reasonable limits
	if maxWidth > 50 {
		maxWidth = 50
	}

	return maxWidth
}

// parseDockerTime parses the CreatedAt field emitted by `docker ps --format {{.CreatedAt}}`.
// Example: "2024-07-20 15:04:05 -0700 MST"
func parseDockerTime(createdAt string) time.Time {
	createdAt = strings.TrimSpace(createdAt)
	if createdAt == "" {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02 15:04:05 -0700 MST", createdAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// sortAgents orders agents with non-running (stopped/created) first by oldest creation time,
// followed by running agents also ordered from oldest to newest. This keeps active containers
// at the bottom while preserving a predictable age-based ordering.
func sortAgents(agents []agentInfo) {
	sort.SliceStable(agents, func(i, j int) bool {
		iRunning := agents[i].status == "Running"
		jRunning := agents[j].status == "Running"
		if iRunning != jRunning {
			return !iRunning && jRunning
		}

		iHasTime := !agents[i].createdAt.IsZero()
		jHasTime := !agents[j].createdAt.IsZero()
		if iHasTime && jHasTime {
			if agents[i].createdAt.Equal(agents[j].createdAt) {
				return agents[i].name < agents[j].name
			}
			return agents[i].createdAt.Before(agents[j].createdAt)
		}
		if iHasTime != jHasTime {
			return iHasTime && !jHasTime
		}

		return agents[i].name < agents[j].name
	})
}

// parseStatus extracts status and time information from Docker status string
// Input examples: "Exited (5) 2 days ago", "Up 3 hours", "Created 1 week ago"
func parseStatus(status string) (statusText, timeText string) {
	status = strings.TrimSpace(status)

	// Handle "Exited (code) time" format
	if strings.HasPrefix(status, "Exited (") {
		// Find the closing parenthesis
		closeIdx := strings.Index(status, ")")
		if closeIdx != -1 && closeIdx+1 < len(status) {
			timePart := strings.TrimSpace(status[closeIdx+1:])
			return "Stopped", timePart
		}
		return "Stopped", ""
	}

	// Handle "Up time" format
	if strings.HasPrefix(status, "Up ") {
		timePart := strings.TrimSpace(status[3:])
		return "Running", timePart
	}

	// Handle "Created time" format
	if strings.HasPrefix(status, "Created ") {
		timePart := strings.TrimSpace(status[8:])
		return "Created", timePart
	}

	// Fallback: return as-is
	return status, ""
}
