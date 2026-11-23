package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
)

const frontendBuildCommand = "dv-build-frontend"

// frontendWatchDisabled reports whether containers should build the frontend
// once instead of running the rolldown watcher.
func frontendWatchDisabled(cfg config.Config) bool {
	return cfg.FrontendWatch != nil && !*cfg.FrontendWatch
}

// syncFrontendModeBestEffort applies the frontendWatch setting to a running
// container. It is skipped entirely when the setting has never been configured,
// so default users pay no extra exec per command.
func syncFrontendModeBestEffort(cmd *cobra.Command, cfg config.Config, name string) {
	if cfg.FrontendWatch == nil {
		return
	}
	if cmd == nil {
		cmd = &cobra.Command{}
	}
	if err := syncFrontendMode(cmd, cfg, name); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: frontend build mode not applied: %v\n", err)
	}
}

func syncFrontendMode(cmd *cobra.Command, cfg config.Config, name string) error {
	img, err := resolveImageConfig(cfg, name)
	if err != nil {
		return err
	}
	if img.Kind != "discourse" {
		return nil
	}
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	script := frontendModeRuntimeScript(frontendWatchDisabled(cfg), img.Workdir)
	output, err := docker.ExecAsRootScriptContext(ctx, name, "/", script)
	if strings.TrimSpace(output) != "" {
		out := cmd.OutOrStdout()
		if err != nil {
			out = cmd.ErrOrStderr()
		}
		fmt.Fprintln(out, strings.TrimSpace(output))
	}
	return err
}

// buildFrontendIfWatchDisabled rebuilds the frontend synchronously after dv
// changes code without restarting the ember service (for example dv import).
func buildFrontendIfWatchDisabled(cmd *cobra.Command, cfg config.Config, name, workdir string) error {
	if !frontendWatchDisabled(cfg) {
		return nil
	}
	img, err := resolveImageConfig(cfg, name)
	if err != nil {
		return err
	}
	if img.Kind != "discourse" {
		return nil
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Building frontend (frontendWatch is false)...")
	out, err := docker.ExecCombinedOutput(name, workdir, nil, []string{frontendBuildCommand})
	if err != nil {
		return fmt.Errorf("frontend build failed: %w\n%s", err, strings.TrimSpace(out))
	}
	return nil
}

// frontendModeRuntimeScript installs dv-build-frontend and makes the ember
// service source /etc/dv/frontend-mode. In build-once mode that file builds the
// frontend and idles instead of starting the watcher. Changing the mode restarts
// a running ember service so it takes effect immediately.
func frontendModeRuntimeScript(buildOnce bool, workdir string) string {
	mode := "# Managed by dv: frontendWatch is enabled, run the watcher.\n"
	if buildOnce {
		mode = frontendBuildOnceMode(workdir)
	}
	return "set -eu\nexec 2>&1\nnew_mode=" + shellQuote(mode) + "\nhelper=" + shellQuote(frontendBuildHelper(workdir)) + "\n" + `
run=/etc/service/ember/run
[ -f "$run" ] || exit 0
mkdir -p /etc/dv /usr/local/bin

printf '%s' "$helper" > /usr/local/bin/dv-build-frontend.dv-tmp
chmod 755 /usr/local/bin/dv-build-frontend.dv-tmp
mv /usr/local/bin/dv-build-frontend.dv-tmp /usr/local/bin/dv-build-frontend

changed=0
printf '%s' "$new_mode" > /etc/dv/frontend-mode.tmp
if cmp -s /etc/dv/frontend-mode.tmp /etc/dv/frontend-mode; then
    rm -f /etc/dv/frontend-mode.tmp
else
    chmod 644 /etc/dv/frontend-mode.tmp
    mv /etc/dv/frontend-mode.tmp /etc/dv/frontend-mode
    changed=1
fi
if ! grep -Fqx '. /etc/dv/frontend-mode # dv frontend mode' "$run"; then
    awk 'NR==1 { print; print ". /etc/dv/frontend-mode # dv frontend mode"; next } { print }' "$run" > "$run.dv-tmp"
    chmod --reference="$run" "$run.dv-tmp"
    mv "$run.dv-tmp" "$run"
    changed=1
fi
[ "$changed" = 1 ] || exit 0

# Docker start may return before runit has initialized its services.
attempts=0
until status=$(cd /etc/service/ember && sv status . 2>&1); do
    attempts=$((attempts + 1))
    if [ "$attempts" -ge 30 ]; then echo "$status" >&2; exit 1; fi
    sleep 1
done
case "$status" in
    run:*)
        echo "Restarting ember to apply frontend build mode (container stays running)."
        (cd /etc/service/ember && sv -w 30 restart . >/dev/null)
        ;;
esac
`
}

func frontendBuildOnceMode(workdir string) string {
	w := shellQuote(workdir)
	return `# Managed by dv: frontendWatch is false. Build the frontend once per service
# start instead of running the rolldown watcher. Rebuild with dv-build-frontend.
if [ -f ` + w + `/frontend/discourse/rolldown.config.mjs ]; then
    log=` + w + `/log/ember.log
    echo "[dv] $(date -Is) building frontend once (frontendWatch is false)" >> "$log"
    if /usr/local/bin/dv-build-frontend >> "$log" 2>&1; then
        echo "[dv] $(date -Is) frontend build finished" >> "$log"
    else
        echo "[dv] $(date -Is) frontend build failed; run dv-build-frontend for details" >> "$log"
    fi
    exec sleep infinity
fi
`
}

func frontendBuildHelper(workdir string) string {
	w := shellQuote(workdir)
	return `#!/bin/bash
# Managed by dv: build the Discourse frontend once. Blocks until the build
# finishes and exits non-zero if it fails. Concurrent builds are serialized.
set -euo pipefail
if [ "$(id -u)" = 0 ]; then
    exec chpst -u discourse:discourse -U discourse:discourse env HOME=/home/discourse USER=discourse "$0" "$@"
fi
if [ ! -f ` + w + `/frontend/discourse/rolldown.config.mjs ]; then
    echo "This Discourse version builds the frontend with ember-cli; its watcher handles rebuilds."
    exit 0
fi
mkdir -p ` + w + `/tmp
exec 9>` + w + `/tmp/dv-build-frontend.lock
flock 9
cd ` + w + `/frontend/discourse
pnpm build
`
}
