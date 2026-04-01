package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

var restartDiscourseCmd = &cobra.Command{
	Use:   "discourse",
	Short: "Smart restart of Discourse services inside the container",
	RunE: func(cmd *cobra.Command, args []string) error {
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}

		name, _ := cmd.Flags().GetString("name")
		if name == "" {
			name = currentAgentName(cfg)
		}

		if !docker.Exists(name) {
			fmt.Fprintf(cmd.OutOrStdout(), "Container '%s' does not exist. Run 'dv start' first.\n", name)
			return nil
		}

		// Ensure container is running
		if !docker.Running(name) {
			fmt.Fprintf(cmd.OutOrStdout(), "Starting container '%s'...\n", name)
			if err := docker.Start(name); err != nil {
				return err
			}
		}

		// Determine workdir from the associated image if known; fall back to selected image
		imgName := cfg.ContainerImages[name]
		var imgCfg config.ImageConfig
		if imgName != "" {
			imgCfg = cfg.Images[imgName]
		} else {
			_, imgCfg, err = resolveImage(cfg, "")
			if err != nil {
				return err
			}
		}
		workdir := imgCfg.Workdir

		// Stop services that exist under /etc/service
		fmt.Fprintf(cmd.OutOrStdout(), "Stopping services (if present)...\n")
		stopScript := `set -e
has_service() { [ -d "/etc/service/$1" ]; }
if has_service sidekiq; then sv force-stop sidekiq || true; fi
if has_service pitchfork; then sv force-stop pitchfork || true; fi
if has_service ember-cli; then sv force-stop ember-cli || true; fi
if has_service caddy; then sv force-stop caddy || true; fi
sleep 1`
		_, _ = docker.ExecAsRoot(name, workdir, nil, []string{"bash", "-lc", stopScript})

		// Start available services
		fmt.Fprintf(cmd.OutOrStdout(), "Starting services (if present)...\n")
		startScript := `set -e
has_service() { [ -d "/etc/service/$1" ]; }
if has_service sidekiq; then sv start sidekiq || true; fi
if has_service pitchfork; then sv start pitchfork || true; fi
if has_service ember-cli; then sv start ember-cli || true; fi
if has_service caddy; then sv start caddy || true; fi
sleep 1`
		_, _ = docker.ExecAsRoot(name, workdir, nil, []string{"bash", "-lc", startScript})

		// Print status only for services that exist
		fmt.Fprintf(cmd.OutOrStdout(), "Service status:\n")
		statusScript := `set -e
services=()
for s in sidekiq pitchfork ember-cli caddy; do
  [ -d "/etc/service/$s" ] && services+=("$s")
done
if [ ${#services[@]} -gt 0 ]; then
  sv status "${services[@]}" || true
else
  echo "No runit services found"
fi`
		if out, err := docker.ExecAsRoot(name, workdir, nil, []string{"bash", "-lc", statusScript}); err == nil {
			fmt.Fprint(cmd.OutOrStdout(), out)
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Discourse services restarted.\n")
		return nil
	},
}

func init() {
	restartDiscourseCmd.Flags().String("name", "", "Container name (defaults to selected or default)")
	restartCmd.AddCommand(restartDiscourseCmd)
}
