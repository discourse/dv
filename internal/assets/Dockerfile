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

RUN mkdir -p /etc/service/pitchfork && \
    tee /etc/service/pitchfork/run > /dev/null <<'EOF'
#!/bin/bash

cd /var/www/discourse
HOME=/home/discourse USER=discourse exec chpst -u discourse:discourse -U discourse:discourse bin/pitchfork >> /var/www/discourse/log/unicorn.log 2>&1
EOF
RUN chmod +x /etc/service/pitchfork/run
RUN tee /etc/service/pitchfork/finish > /dev/null <<'EOF'
#!/bin/bash
pkill -f "unicorn master" || true
pkill -f "unicorn worker" || true
EOF
RUN chmod +x /etc/service/pitchfork/finish

RUN mkdir -p /etc/runit/3.d && \
    tee /etc/runit/3.d/03-pitchfork > /dev/null <<'EOF'
#!/bin/bash
sv stop pitchfork
EOF
RUN chmod +x /etc/runit/3.d/03-pitchfork

RUN mkdir -p /etc/service/ember-cli && \
    tee /etc/service/ember-cli/run > /dev/null <<'EOF'
#!/bin/bash

cd /var/www/discourse
HOME=/home/discourse UNICORN_PORT=9292 PITCHFORK_PORT=9292 USER=discourse exec chpst -u discourse:discourse -U discourse:discourse bin/ember-cli >> /var/www/discourse/log/ember-cli.log 2>&1
EOF
RUN chmod +x /etc/service/ember-cli/run
RUN tee /etc/service/ember-cli/finish > /dev/null <<'EOF'
#!/bin/bash
pkill -f "bin/ember-cli" || true
pkill -f "node.*ember" || true
EOF
RUN chmod +x /etc/service/ember-cli/finish

RUN mkdir -p /etc/runit/3.d && \
    tee /etc/runit/3.d/02-ember-cli > /dev/null <<'EOF'
#!/bin/bash
sv stop ember-cli
EOF
RUN chmod +x /etc/runit/3.d/02-ember-cli

RUN mkdir -p /etc/service/caddy && \
    tee /etc/service/caddy/run > /dev/null <<'EOF'
#!/bin/bash
caddy trust >> /var/log/caddy.log 2>&1
exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile >> /var/log/caddy.log 2>&1
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
    reverse_proxy localhost:4200
}

*.dv.localhost {
    tls internal
    reverse_proxy localhost:4200
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
ENV UNICORN_PORT=9292
ENV PITCHFORK_PORT=9292

ENTRYPOINT ["/sbin/boot"]
