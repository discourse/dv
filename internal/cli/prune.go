package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

// pruneContainer is a container as `docker inspect` reports it.
type pruneContainer struct {
	name       string
	state      string
	owner      string
	createdAt  time.Time
	finishedAt time.Time
}

// lastUsed is when the container last stopped, or when it was created if it
// never ran.
func (c pruneContainer) lastUsed() time.Time {
	if c.finishedAt.IsZero() || c.finishedAt.Year() <= 1 {
		return c.createdAt
	}
	return c.finishedAt
}

type pruneCandidate struct {
	pruneContainer
	idle time.Duration
}

// Seams for tests.
var (
	pruneInspect     = dockerPruneInspect
	pruneUnsavedWork = containerUnsavedWork
	pruneRemove      = func(cmd *cobra.Command, name, configDir string) error {
		return runRemoveInConfig(cmd, []string{name}, true, false, configDir)
	}
	pruneNow = time.Now
)

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove dv containers that have not run for a while",
	Long: `Remove dv containers that are stopped and have not run for longer than
--older-than (default 14d). Without --yes, only lists what would go.

With --yes, each candidate is started briefly and its git checkouts (the
workdir and its plugins) are checked first. A container with uncommitted
changes, stashes or commits not on a remote is kept and reported; extract
that work or remove it with 'dv remove'. The selected and default agents
and names given to --keep are never pruned.`,
	Args: cobra.NoArgs,
	RunE: runPrune,
}

func runPrune(cmd *cobra.Command, _ []string) error {
	olderThanRaw, _ := cmd.Flags().GetString("older-than")
	olderThan, err := parseAge(olderThanRaw)
	if err != nil {
		return err
	}
	yes, _ := cmd.Flags().GetBool("yes")
	keep, _ := cmd.Flags().GetStringSlice("keep")

	configDir, err := xdg.ConfigDir()
	if err != nil {
		return err
	}
	cfg, err := config.LoadOrCreate(configDir)
	if err != nil {
		return err
	}
	containers, err := pruneInspect(cmd.Context())
	if err != nil {
		return err
	}
	protected := append([]string{currentAgentName(cfg), cfg.DefaultContainer}, keep...)
	candidates := selectPruneCandidates(cfg, containers, olderThan, pruneNow(), protected)

	out := cmd.OutOrStdout()
	if len(candidates) == 0 {
		fmt.Fprintf(out, "No dv containers stopped for more than %s\n", olderThanRaw)
		return nil
	}
	if !yes {
		fmt.Fprintf(out, "Would check for unsaved work, then remove (run with --yes):\n")
		for _, c := range candidates {
			fmt.Fprintf(out, "  %s  (last ran %s ago)\n", c.name, humanAge(c.idle))
		}
		return nil
	}

	var removed, kept []string
	var failures []error
	for _, c := range candidates {
		work, err := pruneUnsavedWork(cmd.Context(), cfg, c.name)
		if err != nil {
			kept = append(kept, c.name)
			fmt.Fprintf(cmd.ErrOrStderr(), "Keeping %s: could not check for unsaved work: %v\n", c.name, err)
			continue
		}
		if work != "" {
			kept = append(kept, c.name)
			fmt.Fprintf(out, "Keeping %s: unsaved work\n%s\n", c.name, indent(work, "    "))
			continue
		}
		if err := pruneRemove(cmd, c.name, configDir); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", c.name, err))
			continue
		}
		removed = append(removed, c.name)
	}
	fmt.Fprintf(out, "Pruned %d container(s), kept %d with unsaved work or unchecked\n", len(removed), len(kept))
	return errors.Join(failures...)
}

// selectPruneCandidates returns the dv containers that are stopped, last ran
// longer ago than olderThan, and are not protected, oldest first.
func selectPruneCandidates(cfg config.Config, containers []pruneContainer, olderThan time.Duration, now time.Time, protected []string) []pruneCandidate {
	skip := map[string]bool{}
	for _, name := range protected {
		if name = strings.TrimSpace(name); name != "" {
			skip[name] = true
		}
	}
	var out []pruneCandidate
	for _, c := range containers {
		_, recorded := cfg.ContainerImages[c.name]
		if c.owner != "dv" && !recorded {
			continue
		}
		if skip[c.name] || (c.state != "exited" && c.state != "created") {
			continue
		}
		last := c.lastUsed()
		if last.IsZero() {
			continue
		}
		idle := now.Sub(last)
		if idle <= olderThan {
			continue
		}
		out = append(out, pruneCandidate{pruneContainer: c, idle: idle})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].idle > out[j].idle })
	return out
}

func dockerPruneInspect(ctx context.Context) ([]pruneContainer, error) {
	ids, err := exec.CommandContext(ctx, "docker", "ps", "-aq").Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	fields := strings.Fields(string(ids))
	if len(fields) == 0 {
		return nil, nil
	}
	format := `{{.Name}}{{"\t"}}{{.State.Status}}{{"\t"}}{{index .Config.Labels "com.dv.owner"}}{{"\t"}}{{.Created}}{{"\t"}}{{.State.FinishedAt}}`
	args := append([]string{"inspect", "--format", format}, fields...)
	raw, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("docker inspect: %w", err)
	}
	return parsePruneInspect(string(raw)), nil
}

func parsePruneInspect(raw string) []pruneContainer {
	var out []pruneContainer
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 5 {
			continue
		}
		created, _ := time.Parse(time.RFC3339Nano, parts[3])
		finished, _ := time.Parse(time.RFC3339Nano, parts[4])
		out = append(out, pruneContainer{
			name:       strings.TrimPrefix(parts[0], "/"),
			state:      parts[1],
			owner:      parts[2],
			createdAt:  created,
			finishedAt: finished,
		})
	}
	return out
}

// pruneWorkScript reports, for the workdir and each plugin checkout, what
// would be lost with the container: uncommitted changes, stashes and
// commits on no remote.
const pruneWorkScript = `
for d in "$1" "$1"/plugins/*; do
  [ -e "$d/.git" ] || continue
  status=$(git -C "$d" status --porcelain --untracked-files=normal 2>&1 | head -5)
  stash=$(git -C "$d" stash list 2>&1 | head -3)
  unpushed=$(git -C "$d" log --branches --not --remotes --oneline 2>&1 | head -5)
  if [ -n "$status$stash$unpushed" ]; then
    echo "$d:"
    [ -n "$status" ] && echo "$status" | sed 's/^/  /'
    [ -n "$stash" ] && echo "$stash" | sed 's/^/  stash: /'
    [ -n "$unpushed" ] && echo "$unpushed" | sed 's/^/  unpushed: /'
  fi
done
exit 0
`

// containerUnsavedWork starts the container if needed, runs pruneWorkScript
// in it and stops it again. An empty result means nothing would be lost.
func containerUnsavedWork(ctx context.Context, cfg config.Config, name string) (string, error) {
	imgCfg, err := resolveImageConfig(cfg, name)
	if err != nil {
		return "", err
	}
	workdir := config.EffectiveWorkdir(cfg, imgCfg, name)
	if !docker.Running(name) {
		if out, err := exec.CommandContext(ctx, "docker", "start", name).CombinedOutput(); err != nil {
			return "", fmt.Errorf("docker start: %w: %s", err, strings.TrimSpace(string(out)))
		}
		defer func() {
			_ = docker.StopContext(context.WithoutCancel(ctx), name, nil, nil)
		}()
	}
	out, err := docker.ExecOutputContext(ctx, name, "/", nil, []string{"bash", "-c", pruneWorkScript, "prune", workdir})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// parseAge accepts Go durations plus days and weeks: 36h, 14d, 2w.
func parseAge(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if n, ok := strings.CutSuffix(s, suffix); ok {
			v, err := strconv.Atoi(n)
			if err != nil || v < 0 {
				return 0, fmt.Errorf("invalid age %q", raw)
			}
			return time.Duration(v) * unit, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid age %q (use e.g. 14d, 2w, 36h)", raw)
	}
	return d, nil
}

func humanAge(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
	return fmt.Sprintf("%d hours", int(d.Hours()))
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func init() {
	pruneCmd.Flags().String("older-than", "14d", "Prune containers that last ran longer ago than this (e.g. 14d, 2w, 36h)")
	pruneCmd.Flags().BoolP("yes", "y", false, "Remove the containers instead of listing them")
	pruneCmd.Flags().StringSlice("keep", nil, "Container names never to prune (repeatable)")
}
