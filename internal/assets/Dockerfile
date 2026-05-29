FROM discourse/discourse_dev:release

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update
RUN apt-get install -y --no-install-recommends vim ripgrep zip inotify-tools chromium libnss3-tools curl caddy

RUN chown discourse:discourse /var/www
RUN sudo -H -u discourse /bin/bash -lc 'git clone https://github.com/discourse/discourse.git /var/www/discourse'
RUN sudo -H -u discourse /bin/bash -lc 'cd /var/www/discourse && bundle'
RUN sudo -H -u discourse /bin/bash -lc 'cd /var/www/discourse && pnpm install'

RUN npm i -g @openai/codex
RUN npm i -g @google/gemini-cli
RUN npm i -g @charmland/crush
RUN npm i -g opencode-ai@latest
RUN npm i -g @sourcegraph/amp
RUN npm i -g @github/copilot
RUN npm i -g @musistudio/claude-code-router
RUN sudo -H -u discourse /bin/bash -lc 'curl -fsSL https://claude.ai/install.sh | bash'
RUN sudo -H -u discourse /bin/bash -lc 'curl -LsSf https://aider.chat/install.sh | sh'
RUN sudo -H -u discourse /bin/bash -lc 'curl -fsS https://cursor.com/install | bash'
RUN sudo -H -u discourse /bin/bash -lc 'curl -fsSL https://app.factory.ai/cli | sh'
RUN sudo -H -u discourse /bin/bash -lc 'curl -LsSf https://mistral.ai/vibe/install.sh | bash'
RUN sudo -H -u discourse /bin/bash -lc 'curl -fsSL https://raw.githubusercontent.com/samsaffron/term-llm/main/install.sh | sh'

RUN tee /tmp/seed_users.rb > /dev/null <<'EOF'
u = User.new(username: "admin", email: "admin@example.com")

u.password = "password"
u.name = "Admin"
u.active = true
u.save!(validate: false)

u.grant_admin!
u.change_trust_level!(4) 
u.email_tokens.update_all(confirmed: true)
u.activate

puts "Admin ready: username=#{u.username} email=#{u.email}"

(0..3).each do |i|
  username = "user#{i}"
  email = "#{username}@example.com"

  u = User.new(username: username, email: email)

  u.password = "password"
  u.name = username.capitalize if u.name.blank?
  u.active = true
  u.save!(validate: false)

  tl = i + 1
  u.change_trust_level!(tl) if u.trust_level < tl
  u.email_tokens.update_all(confirmed: true)
  u.activate

  puts "User ready: username=#{u.username} tl=#{u.trust_level}"
end
EOF

RUN tee /tmp/wait-for-services.sh > /dev/null <<'EOF'
#!/bin/bash
set -e
TIMEOUT=120

echo "Waiting for PostgreSQL..."
for i in $(seq 1 $TIMEOUT); do
  # Check socket exists AND pg_isready passes (via socket, not TCP)
  if [ -S /var/run/postgresql/.s.PGSQL.5432 ] && pg_isready -q 2>/dev/null; then
    echo "PostgreSQL ready after ${i}s"
    break
  fi
  if [ $i -eq $TIMEOUT ]; then
    echo "ERROR: PostgreSQL not ready after ${TIMEOUT}s"
    exit 1
  fi
  sleep 1
done

echo "Waiting for Redis..."
for i in $(seq 1 $TIMEOUT); do
  if redis-cli ping 2>/dev/null | grep -q PONG; then
    echo "Redis ready after ${i}s"
    break
  fi
  if [ $i -eq $TIMEOUT ]; then
    echo "ERROR: Redis not ready after ${TIMEOUT}s"
    exit 1
  fi
  sleep 1
done

echo "All services ready!"
EOF
RUN chmod +x /tmp/wait-for-services.sh

RUN /sbin/boot & \
    ( \
      set -e; \
      /tmp/wait-for-services.sh; \
      sudo -H -u discourse /bin/bash -lc 'cd /var/www/discourse && bin/rake db:create'; \
      sudo -H -u discourse /bin/bash -lc 'cd /var/www/discourse && bin/rake db:migrate'; \
      sudo -H -u discourse /bin/bash -lc 'cd /var/www/discourse && RAILS_ENV=test bin/rake db:migrate'; \
      sudo -H -u discourse /bin/bash -lc 'cd /var/www/discourse && bin/rails r /tmp/seed_users.rb' \
    ); \
    status=$?; \
    pkill -f "/sbin/boot" || true; \
    exit $status

RUN sudo -H -u discourse /bin/bash -lc "cd /var/www/discourse && npx playwright install-deps && npx playwright install"

RUN mkdir -p /etc/service/rails && \
    tee /etc/service/rails/run > /dev/null <<'EOF'
#!/bin/bash

cd /var/www/discourse
HOME=/home/discourse USER=discourse UNICORN_BIND_ALL=1 exec chpst -u discourse:discourse -U discourse:discourse bin/rails s >> /var/www/discourse/log/rails.log 2>&1
EOF
RUN chmod +x /etc/service/rails/run
RUN tee /etc/service/rails/finish > /dev/null <<'EOF'
#!/bin/bash
pkill -f "pitchfork" || true
EOF
RUN chmod +x /etc/service/rails/finish

RUN mkdir -p /etc/runit/3.d && \
    tee /etc/runit/3.d/03-rails > /dev/null <<'EOF'
#!/bin/bash
sv stop rails
EOF
RUN chmod +x /etc/runit/3.d/03-rails

RUN mkdir -p /etc/service/ember && \
    tee /etc/service/ember/run > /dev/null <<'EOF'
#!/bin/bash

cd /var/www/discourse
# Pick the JS dev command based on which Discourse layout is on disk:
#   - rolldown (post-PR#35963): bin/dev --only ember runs the rolldown watcher
#   - legacy ember-cli:         bin/ember-cli serves the proxy on :4200
if [ -f /var/www/discourse/frontend/discourse/rolldown.config.mjs ]; then
    js_cmd=("bin/dev" "--only" "ember")
else
    js_cmd=("bin/ember-cli")
fi
HOME=/home/discourse UNICORN_PORT=3000 USER=discourse exec chpst -u discourse:discourse -U discourse:discourse "${js_cmd[@]}" >> /var/www/discourse/log/ember.log 2>&1
EOF
RUN chmod +x /etc/service/ember/run
RUN tee /etc/service/ember/finish > /dev/null <<'EOF'
#!/bin/bash
pkill -f "bin/ember-cli" || true
pkill -f "bin/dev" || true
pkill -f "node.*ember" || true
pkill -f "rolldown" || true
EOF
RUN chmod +x /etc/service/ember/finish

RUN mkdir -p /etc/runit/3.d && \
    tee /etc/runit/3.d/02-ember > /dev/null <<'EOF'
#!/bin/bash
sv stop ember
EOF
RUN chmod +x /etc/runit/3.d/02-ember

RUN mkdir -p /etc/service/caddy && \
    tee /usr/local/bin/dv-caddy-run > /dev/null <<'EOF'
#!/bin/bash
set -euo pipefail

log=/var/log/caddy.log
runtime_caddyfile=/etc/caddy/Caddyfile.runtime

# Keep the historical localhost wildcard, and add any hostname(s) passed by dv
# when the local proxy is enabled (for example: sideload.dev.home.arpa and
# *.dev.home.arpa). This lets in-container browsers reach the same HTTPS
# hostname Discourse advertises without hand-editing /etc/caddy/Caddyfile.
site_addresses="*.dv.localhost"
if [[ -n "${DV_CADDY_HOSTS:-}" ]]; then
    site_addresses="${site_addresses}, ${DV_CADDY_HOSTS}"
elif [[ -n "${DV_LOCAL_PROXY_HOST:-}" ]]; then
    site_addresses="${site_addresses}, ${DV_LOCAL_PROXY_HOST}"
fi

https_block=""
https_enabled="false"
if [[ "${DV_LOCAL_PROXY_SCHEME:-}" == "https" || -n "${DV_LOCAL_PROXY_HTTPS_PORT:-}" ]]; then
    https_enabled="true"
    https_addresses="${site_addresses}"
    if [[ -n "${DV_LOCAL_PROXY_HTTPS_PORT:-}" && "${DV_LOCAL_PROXY_HTTPS_PORT}" != "443" ]]; then
        IFS=',' read -ra caddy_hosts <<< "${site_addresses}"
        for caddy_host in "${caddy_hosts[@]}"; do
            caddy_host="${caddy_host#${caddy_host%%[![:space:]]*}}"
            caddy_host="${caddy_host%${caddy_host##*[![:space:]]}}"
            [[ -z "${caddy_host}" ]] && continue
            https_addresses="${https_addresses}, ${caddy_host}:${DV_LOCAL_PROXY_HTTPS_PORT}"
        done
    fi
    https_block="${https_addresses} {
    tls internal
    reverse_proxy localhost:3000
}"
fi

extra_http_listener=""
if [[ -n "${DV_LOCAL_PROXY_HTTP_PORT:-}" && "${DV_LOCAL_PROXY_HTTP_PORT}" != "80" ]]; then
    extra_http_listener=":${DV_LOCAL_PROXY_HTTP_PORT} {
    reverse_proxy localhost:3000
}"
fi

cat > "${runtime_caddyfile}" <<CADDY
{
    admin off
}

:80 {
    reverse_proxy localhost:3000
}

${extra_http_listener}

${https_block}
CADDY

caddy fmt --overwrite "${runtime_caddyfile}" >> "${log}" 2>&1 || true

caddy_pid=""
tmpdir=""

cleanup_tmpdir() {
    if [[ -n "${tmpdir}" && -d "${tmpdir}" ]]; then
        rm -rf "${tmpdir}"
    fi
}

stop_caddy() {
    if [[ -n "${caddy_pid}" ]] && kill -0 "${caddy_pid}" >/dev/null 2>&1; then
        kill -TERM "${caddy_pid}" >/dev/null 2>&1 || true
        wait "${caddy_pid}" || true
    fi
}

on_exit() {
    status=$?
    trap - EXIT
    cleanup_tmpdir
    if [[ "${status}" -ne 0 ]]; then
        stop_caddy
    fi
    exit "${status}"
}
trap on_exit EXIT

caddy run --config "${runtime_caddyfile}" --adapter caddyfile >> "${log}" 2>&1 &
caddy_pid=$!

terminate() {
    trap - TERM INT
    stop_caddy
    exit 0
}
trap terminate TERM INT

if [[ "${https_enabled}" == "true" ]]; then
    root_crt=/root/.local/share/caddy/pki/authorities/local/root.crt
    for _ in {1..300}; do
        if [[ -f "${root_crt}" ]]; then
            break
        fi
        if ! kill -0 "${caddy_pid}" >/dev/null 2>&1; then
            wait "${caddy_pid}"
            exit $?
        fi
        sleep 0.1
    done

    if [[ -f "${root_crt}" ]]; then
        # Trust Caddy's local CA system-wide without invoking `caddy trust`: this
        # service runs Caddy with `admin off`, and `caddy trust` may try to fetch the
        # root from the disabled admin API. Chromium also reads per-user NSS DBs, so
        # import the same root into both locations used by the discourse user.
        if command -v update-ca-certificates >/dev/null 2>&1; then
            install -m 0644 "${root_crt}" /usr/local/share/ca-certificates/caddy-local-root.crt >> "${log}" 2>&1 || true
            update-ca-certificates >> "${log}" 2>&1 || true
        fi

        nickname="Caddy Local Authority"

        if command -v certutil >/dev/null 2>&1; then
            tmpdir="$(mktemp -d)"
            chmod 0755 "${tmpdir}"
            root_crt_copy="${tmpdir}/caddy-local-root.crt"
            nss_password_file="${tmpdir}/caddy-nss-empty-password"

            install -m 0644 "${root_crt}" "${root_crt_copy}"
            printf '\n' > "${nss_password_file}"
            chown discourse:discourse "${nss_password_file}"

            for nssdb in /home/discourse/.local/share/pki/nssdb /home/discourse/.pki/nssdb; do
                install -d -o discourse -g discourse "$(dirname "${nssdb}")" "${nssdb}"
                if [[ ! -f "${nssdb}/cert9.db" ]]; then
                    HOME=/home/discourse USER=discourse chpst -u discourse:discourse -U discourse:discourse \
                        certutil -d "sql:${nssdb}" -N -f "${nss_password_file}" >> "${log}" 2>&1 || true
                fi
                if HOME=/home/discourse USER=discourse chpst -u discourse:discourse -U discourse:discourse \
                    certutil -d "sql:${nssdb}" -L -n "${nickname}" >/dev/null 2>&1; then
                    HOME=/home/discourse USER=discourse chpst -u discourse:discourse -U discourse:discourse \
                        certutil -d "sql:${nssdb}" -D -n "${nickname}" >> "${log}" 2>&1 || true
                fi
                HOME=/home/discourse USER=discourse chpst -u discourse:discourse -U discourse:discourse \
                    certutil -d "sql:${nssdb}" -A -t "C,," -n "${nickname}" -i "${root_crt_copy}" -f "${nss_password_file}" >> "${log}" 2>&1 || \
                    echo "Failed to import ${nickname} into ${nssdb}" >> "${log}"
            done

            rm -f "${root_crt_copy}" "${nss_password_file}"
        fi
    else
        echo "Timed out waiting for ${root_crt}" >> "${log}"
    fi
fi

wait "${caddy_pid}"
EOF
RUN chmod +x /usr/local/bin/dv-caddy-run

RUN mkdir -p /etc/service/caddy && \
    tee /etc/service/caddy/run > /dev/null <<'EOF'
#!/bin/bash
exec /usr/local/bin/dv-caddy-run
EOF
RUN chmod +x /etc/service/caddy/run
RUN tee /etc/service/caddy/finish > /dev/null <<'EOF'
#!/bin/bash
pkill -f "caddy" || true
EOF
RUN chmod +x /etc/service/caddy/finish

RUN mkdir -p /etc/caddy && \
    tee /etc/caddy/Caddyfile > /dev/null <<'EOF'
{
    admin off
}

:80 {
    reverse_proxy localhost:3000
}

*.dv.localhost {
    tls internal
    reverse_proxy localhost:3000
}
EOF

RUN mkdir -p /etc/runit/3.d && \
    tee /etc/runit/3.d/01-caddy > /dev/null <<'EOF'
#!/bin/bash
sv stop caddy
EOF
RUN chmod +x /etc/runit/3.d/01-caddy

ENV LEFTHOOK=1
ENV DISCOURSE_DEV_ALLOW_ANON_TO_IMPERSONATE=1
# allows qunit to work without CI being set to 1 which breaks a bunch of agents
ENV DISCOURSE_DISABLE_BROWSER_SANDBOX=1
ENV TESTEM_DEFAULT_BROWSER=Chromium
ENV CHROME_BIN=/usr/bin/chromium
ENV UNICORN_PORT=3000
# Let clients hit Rails (on 3000) directly even when the ember-cli proxy is
# running. On pre-rolldown Discourse this stops Rails from forcing a redirect
# to the ember-cli dev server.
ENV ALLOW_EMBER_CLI_PROXY_BYPASS=1

ENTRYPOINT ["/sbin/boot"]
