package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/localproxy"
	"dv/internal/xdg"
)

// Lifecycle synchronization must not prevent entering an otherwise usable
// container. Explicit hostname commands still report failures as errors.
func syncContainerHostnamesBestEffort(cmd *cobra.Command, cfg config.Config, name string) {
	if cmd == nil {
		cmd = &cobra.Command{}
	}
	err := withHostnameOperationLock(cmd, func() error {
		dir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		latest, err := config.LoadOrCreate(dir)
		if err != nil {
			return err
		}
		// Keep the caller's image context, but never apply a stale hostname
		// snapshot after another command changed aliases while startup ran.
		cfg.HostnameAliases = latest.HostnameAliases
		cfg.HostnameRemovals = latest.HostnameRemovals
		cfg.LabelOverrides = latest.LabelOverrides
		cfg.LocalProxy = latest.LocalProxy
		if !cfg.LocalProxy.Enabled {
			return nil
		}
		projectionErr := config.PublishProxyAliases(dir)
		applyErr := syncContainerHostnames(cmd, cfg, name)
		var routeErr error
		if localproxy.Running(cfg.LocalProxy) {
			routeErr = reconcileHostnameRoutes(dir, cfg, name)
		}
		return errors.Join(projectionErr, applyErr, routeErr)
	})
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: hostname synchronization pending: %v\n", err)
	}
}

// syncContainerHostnames updates the existing container, never its lifecycle.
// An empty (but present) alias list also synchronizes removals queued while stopped.
func syncContainerHostnames(cmd *cobra.Command, cfg config.Config, name string) error {
	if cmd == nil {
		cmd = &cobra.Command{}
	}
	aliases, managed := cfg.HostnameAliases[name]
	if !managed {
		return nil
	}
	img, err := resolveImageConfig(cfg, name)
	if err != nil {
		return err
	}
	if img.Kind != "discourse" {
		return nil
	}
	primary, err := containerPrimaryHostname(cfg, name)
	if err != nil {
		return err
	}
	hosts := append([]string{primary}, aliases...)
	env := "export DISCOURSE_HOSTNAME=" + shellQuote(primary) + "\n" +
		"export RAILS_DEVELOPMENT_HOSTS=" + shellQuote(strings.Join(hosts, ",")) + "\n" +
		"export DV_CADDY_HOSTS=" + shellQuote(caddyHostsForLocalProxy(strings.Join(hosts, ", "), cfg.LocalProxy.Hostname)) + "\n"
	script := hostnameRuntimeScript(env, strings.Join(hosts, " "))
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 180*time.Second)
	defer cancel()
	output, err := docker.ExecAsRootScriptContext(ctx, name, "/", script)
	if strings.TrimSpace(output) != "" {
		out := cmd.OutOrStdout()
		if err != nil {
			out = cmd.ErrOrStderr()
		}
		fmt.Fprintln(out, strings.TrimSpace(output))
	}
	if err != nil {
		return fmt.Errorf("hostname aliases saved, but applying them inside %s failed: %w (retry with dv start %s)", name, err, name)
	}
	return nil
}

func hostnameRuntimeScript(env, hosts string) string {
	return "set -eu\nexec 2>&1\nnew_env=" + shellQuote(strings.TrimRight(env, "\n")) + "\nnew_hosts=" + shellQuote(hosts) + "\n" + `
mkdir -p /etc/dv
exec 9>/etc/dv/hostname.lock
command -v flock >/dev/null || { echo "Cannot apply hostname settings: missing flock" >&2; exit 1; }
flock -w 30 9

# Validate the managed service layout before changing anything.
command -v sv >/dev/null || { echo "Cannot apply hostname settings: missing sv" >&2; exit 1; }
for service in rails caddy; do
    if [ ! -f "/etc/service/$service/run" ]; then
        echo "Cannot apply hostname settings: missing /etc/service/$service/run" >&2
        exit 1
    fi
done

# Older stock images use a broad match that kills the runsv caddy supervisor
# as well as the server. Repair only that known command; preserve custom hooks.
finish=/etc/service/caddy/finish
if [ -f "$finish" ] && grep -Fqx 'pkill -f "caddy" || true' "$finish"; then
    sed 's/^pkill -f "caddy" || true$/pkill -x caddy || true/' "$finish" > "$finish.dv-tmp"
    chmod --reference="$finish" "$finish.dv-tmp"
    mv "$finish.dv-tmp" "$finish"
fi

# Persist overrides in the writable layer, not immutable Docker Config.Env.
mkdir -p /etc/dv
printf '%s' "$new_env" > /etc/dv/hostname-env.tmp
chmod 644 /etc/dv/hostname-env.tmp
mv /etc/dv/hostname-env.tmp /etc/dv/hostname-env

# Replace only dv-managed hostnames; preserve unrelated entries on shared lines.
old_hosts=$(cat /etc/dv/hostname-hosts 2>/dev/null || true)
# Also account for aliases originally injected when the container was created.
initial_hosts=$(printf '%s' "${RAILS_DEVELOPMENT_HOSTS:-}" | tr ',' ' ')
awk -v managed="$old_hosts $new_hosts $initial_hosts" '
BEGIN { n=split(managed, names, " "); for(i=1;i<=n;i++) owned[names[i]]=1 }
/^[[:space:]]*#/ || NF < 2 { print; next }
{ line=$1; count=0; comment=""; for(i=2;i<=NF;i++) { if ($i ~ /^#/) { for(j=i;j<=NF;j++) comment=comment " " $j; break }; if (!($i in owned)) { line=line " " $i; count++ } }
  if(count) print line comment }
' /etc/hosts > /etc/dv/hosts.tmp
printf '127.0.0.1 %s # dv hostname aliases\n' "$new_hosts" >> /etc/dv/hosts.tmp
# /etc/hosts is a Docker bind mount: write through it rather than rename it.
cat /etc/dv/hosts.tmp > /etc/hosts
rm /etc/dv/hosts.tmp
printf '%s\n' "$new_hosts" > /etc/dv/hostname-hosts

# Preserve existing launchers, adding only a persistent environment source.
for service in rails caddy; do
    run=/etc/service/$service/run
    changed=0
    applied=/etc/dv/hostname-$service-applied
    if [ ! -f "$applied" ] || [ "$(cat "$applied")" != "$new_env" ]; then changed=1; fi
    if ! grep -Fqx '. /etc/dv/hostname-env # dv hostname aliases' "$run"; then
        awk 'NR==1 { print; print ". /etc/dv/hostname-env # dv hostname aliases"; next } { print }' "$run" > "$run.dv-tmp"
        chmod --reference="$run" "$run.dv-tmp"
        mv "$run.dv-tmp" "$run"
        changed=1
    fi
    if [ "$changed" = 1 ]; then
        # Docker start may return before runit has initialized its services.
        attempts=0
        until status=$(cd "/etc/service/$service" && sv status . 2>&1); do
            attempts=$((attempts + 1))
            if [ "$attempts" -ge 30 ]; then echo "$status" >&2; exit 1; fi
            sleep 1
        done
        case "$status" in
            run:*)
                echo "Restarting $service to apply hostname aliases (container stays running)."
                # Keep service names out of sv's argv too: Caddy's finish hook
                # uses pkill -f caddy, which would also terminate sv itself.
                (cd "/etc/service/$service" && sv -w 30 restart .)
                ;;
        esac
        printf '%s' "$new_env" > "$applied"
    fi
done
`
}
