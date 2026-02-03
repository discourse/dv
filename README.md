# Discourse AI Agent Container

A Docker-based development environment for AI agents with Discourse.

## Overview

This project provides a containerized development environment that includes:
- Discourse development setup
- Essential developer tools (vim, ripgrep)
- Ready-to-use database configuration, fully migrated dev/test databases
- Various AI helpers preinstalled in the image (Claude, Codex, Aider, Gemini)
- Multi-agent container management via `dv` top-level commands (`list`, `new`, `select`, `rename`) and the `agent` group
 - Embedded Dockerfile managed by the CLI with safe override mechanisms

## Prerequisites

- Docker installed on your system
- Go 1.22+
- Optional: GitHub CLI (`gh`) if you want to use `dv extract`’s default cloning behavior

## Installation

### Using the install script (recommended)

Install the latest release for macOS or Linux with a single command:

```bash
curl -sSfL https://raw.githubusercontent.com/SamSaffron/dv/main/install.sh | sh
```

The script downloads the correct binary for your platform and installs it to `~/.local/bin` (create it if missing). After it finishes, run `dv version` to confirm that the binary is on your `PATH`.

To pin a specific release or control the install location:

```bash
# install a specific tag
curl -sSfL https://raw.githubusercontent.com/SamSaffron/dv/main/install.sh | sh -s -- --version v0.3.0

# install without sudo
curl -sSfL https://raw.githubusercontent.com/SamSaffron/dv/main/install.sh | sh -s -- --install-dir ~/.local/bin
```

You can also set the `DV_INSTALL_DIR` environment variable to change the default target directory. If `~/.local/bin` (or your custom path) isn’t on your `PATH`, add it in your shell profile, e.g. `export PATH="$HOME/.local/bin:$PATH"`.

`dv` automatically checks for updates once per day in the background. When a newer release is published you’ll see a warning; run `dv upgrade` to install it in place without re-running the shell script. Update metadata is cached at `${XDG_CONFIG_HOME}/dv/update-state.json`.

### Build from source

If you’re hacking on `dv`, build the binary directly:

```bash
go build
```

The resulting binary is written to the repository root (run it via `./dv`).

## Quick Start

With `dv` installed (either via the script or `go build`), run the CLI directly from your shell. If you’re using the locally built binary in this repository, replace `dv` with `./dv` in the commands below.

1. Build the Docker image:
   ```bash
   dv build
   ```

2. Start the container:
   ```bash
   dv start
   ```
3. Enter the container, or run a one-off command without opening a shell:
   ```bash
   dv enter
   # run a single command
   dv run -- bin/rails c
   ```
4. Extract changes from the container (when ready to create a PR):
   ```bash
   dv extract
   # or extract changes for a specific plugin (with TAB completion)
   dv extract plugin discourse-akismet
   ```

Optional: manage multiple named containers ("agents"):
```bash
dv new my_project     # create and select a new agent
dv list               # show all agents for the selected image
dv select my_project  # select an existing agent
dv rename old new     # rename an agent
```

## dv Commands

### dv build
Build the Docker image (defaults to tag `ai_agent`).

```bash
dv build [--no-cache] [--build-arg KEY=VAL] [--rm-existing]
```

Notes:
- Uses an embedded `Dockerfile` managed under your XDG config directory. On each build, the CLI ensures the materialized `Dockerfile` matches the embedded version via a SHA file.
- Override precedence:
  1) `DV_DOCKERFILE=/absolute/path/to/Dockerfile`
  2) `${XDG_CONFIG_HOME}/dv/Dockerfile.local`
  3) Embedded default (materialized to `${XDG_CONFIG_HOME}/dv/Dockerfile`)
  The command prints which Dockerfile path it used.
- BuildKit/buildx is enabled by default (`docker buildx build --load`). The CLI automatically falls back to legacy `docker build` if buildx is unavailable.
- Opt-out controls: `--classic-build` forces legacy `docker build`, and `--builder NAME` targets a specific buildx builder (remote builders, Docker Build Cloud, etc.).

### dv pull
Pull a published image/tag instead of building locally.

```bash
dv pull [IMAGE_NAME]
```

### dv image
Manage image definitions, workdirs, ports, and Dockerfile sources.

```bash
dv image list
dv image select NAME
dv image show
```

### dv start
Create or start the container for the selected image (no shell).

```bash
dv start [--reset] [--name NAME] [--image NAME] [--host-starting-port N] [--container-port N]
```

Notes:
- Maps host `4201` → container `4200` by default (Ember CLI dev server). Override with flags.
- Performs a pre-flight check and picks the next free port if needed.

### dv stop
Stop the selected or specified container.

```bash
dv stop [--name NAME]

# Restart the container
dv restart [--name NAME]

# Restart only Discourse services (Unicorn/Sidekiq)
dv restart discourse [--name NAME]
```

### dv reset
Reset the development environment (databases or git state).

```bash
# Reset databases (default behavior)
dv reset [--name NAME]
dv reset db [--name NAME]

# Reset git state (discard local changes, sync with upstream)
dv reset git [--name NAME]
```

Notes for `dv reset` / `dv reset db`:
- Stops Discourse services.
- Resets the development and test databases.
- Runs migrations and seeds test data.
- Restarts services.

Notes for `dv reset git`:
- Discards local code changes in the container.
- Syncs with the upstream branch.
- Reinstalls dependencies and runs migrations.

### dv enter
Attach to the running container as user `discourse` in the workdir and open an interactive shell.

```bash
dv enter [--name NAME]
```

Notes:
- Copies any configured host files into the container before launching the shell (see `copyRules` under config).

### dv run
Run a non-interactive command inside the running container (defaults to the `discourse` user).

```bash
dv run [--name NAME] [--root] -- CMD [ARGS...]
```

Notes:
- Same file-copy behavior as `dv enter`; run `dv run -- <command>` to execute without opening a shell.
- Pass `--root` to execute as `root` inside the container.

### dv run-agent (alias: ra)
Run an AI agent inside the container with a prompt.

```bash
dv run-agent [--name NAME] AGENT [-- ARGS...|PROMPT ...]
# alias
dv ra codex Write a migration to add foo to users

# interactive mode
dv ra codex

# use a file as the prompt (useful for long instructions)
dv ra codex ./prompts/long-instructions.txt
dv ra codex ~/notes/feature-plan.md

# pass raw args directly to the agent (no prompt wrapping)
dv ra aider -- --yes -m "Refactor widget"
```

Notes:
- Autocompletes common agents: `codex`, `aider`, `claude`, `gemini`, `crush`, `cursor`, `opencode`, `amp`.
- If no prompt is provided, an inline TUI opens for multi-line input (Ctrl+D to run, Esc to cancel).
- You can pass a regular file path as the first argument after the agent (e.g. `dv ra codex ./plan.md`). The file will be read on the host and its contents used as the prompt. If the argument is not a file, the existing prompt behavior is used.
- Filename/path completion is supported when you start typing a path (e.g. `./`, `../`, `/`, or include a path separator).
- Agent invocation is rule-based (no runtime discovery). Use `--` to pass raw args unchanged (e.g., `dv ra codex -- --help`).

### dv mail
Run MailHog and tunnel it to localhost.

```bash
dv mail [--port 8025] [--host-port 8025]
```

Allows you to access MailHog from your browser (e.g., http://localhost:8025) to inspect emails sent by Discourse. Press Ctrl+C to stop the process and the tunnel.

### dv tui
Launch an interactive TUI to manage containers, images, and run commands.

```bash
dv tui
```

### dv import
Push local commits or uncommitted work from the host repository into the running container.

```bash
dv import [--base main]
```

### dv update agents
Refresh the preinstalled AI agents inside the container (Codex, Gemini, Crush, Claude, Aider, Cursor, OpenCode).

```bash
dv update agents [--name NAME]
```

Notes:
- Starts the container if needed before running updates.
- Re-runs the official install scripts or package managers to pull the latest versions.

### dv remove
Remove the container and optionally the image.

```bash
dv remove [--image] [--name NAME]
```

### Agent management
Manage multiple containers for the selected image; selection is stored in XDG config. These are the preferred top-level commands; the old `dv agent` group has been removed.

```bash
dv list
dv new [NAME]
dv select NAME
dv rename OLD NEW
```

### Templates
Provision containers with pre-defined configurations using YAML templates. This is useful for setting up specific environments, installing plugins/themes, or applying site settings automatically.

```bash
# Create a new agent from a local template
dv new my-feature --template ./templates/stable.yaml

# Create a new agent from a URL
dv new my-feature --template https://raw.githubusercontent.com/discourse/dv/main/templates/full.yaml
```

Templates support:
- **Discourse Configuration**: Specify branches, PRs, or custom repos.
- **Plugins & Themes**: Automatically clone plugins and install/watch themes.
- **Site Settings**: Set Discourse settings (title, theme, experimental features) on boot.
- **Copy Rules**: Sync host files (like `.gitconfig` or API keys) into the container.
- **Provisioning**: Run arbitrary bash commands via `on_create`.
- **MCP Servers**: Register Model Context Protocol servers for AI agents.

See [templates/full.yaml](./templates/full.yaml) for a complete example of all available features.

### dv extract
Copy modified files from the running container’s `/var/www/discourse` into a local clone and create a new branch at the container’s HEAD.

```bash
dv extract [--name NAME] [--sync] [--debug]
```

By default, the destination is `${XDG_DATA_HOME}/dv/discourse_src`. When a container uses a custom workdir (for example, a theme under `/home/discourse/winter-colors`), the extract target becomes `${XDG_DATA_HOME}/dv/<workdir-slug>_src` so each workspace mirrors into its own folder.

`--sync` keeps the container and host codebases synchronized after the initial extract by watching for changes in both environments (press `Ctrl+C` to exit). `--debug` adds verbose logging while in sync mode. These flags cannot be combined with `--chdir` or `--echo-cd`.

Note: sync mode requires `inotifywait` to be available inside the container (included in latest Dockerfile used here).

Examples:

```bash
# Perform a one-off extract
dv extract

# Start continuous two-way sync with verbose logging
dv extract --sync --debug
```

### dv pr
Checkout a GitHub pull request in the container and reset the development environment.

```bash
dv pr [--name NAME] [--no-reset] NUMBER
```

Notes:
- Fetches and checks out the specified PR into a local branch.
- Performs a full database reset and migration (development and test databases).
- Reinstalls dependencies (bundle and pnpm).
- Seeds test users.
- Use `--no-reset` to skip DB reset and migrations (only checkout and reinstall deps).
- Supports TAB completion with PR numbers and titles from GitHub API.
- Only works with containers using the `discourse` image kind.

Examples:
```bash
# Checkout PR #12345
dv pr 12345

# Checkout without resetting DB
dv pr --no-reset 12345

# Use TAB completion to search and select a PR
dv pr <TAB>
```

### dv branch
Checkout a git branch in the container and reset the development environment.

```bash
dv branch [--name NAME] [--no-reset] [--new] BRANCH
```

Notes:
- Checks out the specified branch and pulls latest changes.
- Performs a full database reset and migration (development and test databases).
- Reinstalls dependencies (bundle and pnpm).
- Seeds test users.
- Use `--no-reset` to skip DB reset and migrations (only checkout and reinstall deps).
- Use `--new` to create a new branch from origin/main (or origin/master) if the branch does not exist on remote.
- Supports TAB completion(e.g., `dv branch me<TAB>` queries only branches starting with "me").
- Only works with containers using the `discourse` image kind.

Examples:
```bash
# Checkout main branch
dv branch main

# Use TAB completion to list and select a branch
dv branch <TAB>

# Checkout a feature branch
dv branch feature/my-feature

# Create a new local branch for development
dv branch --new my-new-feature

# Quickly switch branches without resetting DB
dv branch --no-reset main
```

### dv extract plugin
Extract changes for a single plugin from the running container. This is useful when a plugin is its own git repository under `/var/www/discourse/plugins`.

```bash
dv extract plugin <name> [--name NAME] [--chdir] [--echo-cd]
```

Notes:
- Requires the container to be running to discover plugins.
- TAB completion suggests plugin names under `/var/www/discourse/plugins` that are separate git repositories from the core Discourse repo.
- Destination is `${XDG_DATA_HOME}/dv/<PLUGIN>_src`.
- If the plugin is a git repo with a remote, dv clones it and checks out a branch/commit matching the container; only modified/untracked files are copied over.
- If the plugin has no git remote or isn’t a git repo, dv copies the whole directory to `<PLUGIN>_src`.
- `--chdir` opens a subshell in the extracted directory on completion. `--echo-cd` prints a `cd <path>` line to stdout (suitable for `eval`).

Examples:
```bash
# Autocomplete plugin name
dv extract plugin <TAB>

# Extract changes for akismet plugin
dv extract plugin discourse-akismet

# Jump into the extracted repo afterwards
dv extract plugin discourse-akismet --chdir

# Use in command substitution to cd silently
eval "$(dv extract plugin discourse-akismet --echo-cd)"
```

### dv extract theme
Extract changes for a theme from `/home/discourse` inside the container.

```bash
dv extract theme <name> [--name NAME] [--sync] [--debug] [--chdir] [--echo-cd]
```

Notes:
- Requires the container to be running to discover themes.
- TAB completion suggests theme directories under `/home/discourse` that are git repositories.
- Destination is `${XDG_DATA_HOME}/dv/<THEME>_src`.
- `--sync` enables continuous bidirectional synchronization (press Ctrl+C to stop).
- `--chdir` opens a subshell in the extracted directory on completion. `--echo-cd` prints a `cd <path>` line to stdout (suitable for `eval`).

Examples:
```bash
# Extract a theme
dv extract theme winter-colors

# Start continuous sync for theme development
dv extract theme winter-colors --sync

# Jump into the extracted repo afterwards
dv extract theme winter-colors --chdir
```

### dv config
Read/write config stored at `${XDG_CONFIG_HOME}/dv/config.json`.

```bash
dv config get KEY
dv config set KEY VALUE
dv config show
```

#### AI Configuration (LLMs)
Use `dv config ai` to launch a TUI for configuring Discourse AI LLM providers (OpenAI, Anthropic, Bedrock, etc.) and models. It automatically detects API keys from your host environment variables.

#### AI Tool Workspace
Use `dv config ai-tool [NAME]` to scaffold a directory under `/home/discourse/ai-tools` for developing custom Discourse AI tools. It includes `tool.yml` (metadata), `script.js` (logic), and `bin/test` / `bin/sync` helpers.

#### Theme bootstrap
Use `dv config theme [REPO]` to prepare a theme workspace inside the running container. Running it with no arguments prompts for a name **and** whether you’re building a full theme or component, installs the `discourse_theme` gem, scaffolds a minimal theme under `/home/discourse/<name>`, writes an `AGENTS.md` brief for AI tools, and updates the workdir override so `dv enter` drops you there. Supplying a git URL or `owner/repo` slug clones the existing theme instead of generating a skeleton, while still installing the gem, writing `AGENTS.md`, and configuring the watcher. Each workspace also receives a `theme-watch-<slug>` runit service that runs `discourse_theme watch` with an API key that’s automatically bound to the first admin user; restart it anytime with `sv restart theme-watch-<slug>` inside the container. Pass `--theme-name` (and optionally `--kind theme|component`) to skip the interactive prompts, and `--verbose` if you want to see every helper command that runs (handy when debugging API key or watcher issues).

#### Site Settings
Use `dv config site_settings FILENAME.yaml` to apply Discourse site settings from a YAML file. Supports 1Password integration via `op://` references for sensitive values.

#### Local proxy (NAME.dv.localhost)
Run `dv config local-proxy` to build and start a small reverse proxy container (`dv-local-proxy` by default) that maps each new agent to `NAME.dv.localhost` instead of host ports like `localhost:4201`. By default, the proxy listens on localhost only (port 80 for HTTP, 2080 for admin API) for security. Use `--public` to bind to all network interfaces. Use `--https` to enable HTTPS on port 443 via a local mkcert certificate (HTTP will redirect to HTTPS). The proxy registers containers as you create/start them and injects hostname env vars so assets resolve correctly. Stop or remove the proxy container to go back to host-port URLs; only containers created while the proxy is running adopt the hostname.

#### Claude Code Router (CCR)
Use `dv config ccr` to bootstrap Claude Code Router presets via OpenRouter/OpenAI rankings.

#### Copying host files before enter/run-agent
Use `copyRules` in your config to copy host files into the container. Each rule sets a host path (supports `~`, env vars, and globs) and a container destination, plus optional `agents` to only copy when that agent is run via `dv run-agent`. Unscoped rules run for `dv enter`/`dv run`; agent-scoped rules skip those commands.

```json
{
  "copyRules": [
    { "host": "~/.codex/auth.json",      "container": "/home/discourse/.codex/auth.json",      "agents": ["codex"] },
    { "host": "~/.gemini/GEMINI.md",     "container": "/home/discourse/.gemini/GEMINI.md",     "agents": ["gemini"] },
    { "host": "~/.gemini/*.json",        "container": "/home/discourse/.gemini/",              "agents": ["gemini"] },
    { "host": "~/.gemini/google_account_id",     "container": "/home/discourse/google_account_id",     "agents": ["gemini"] }
  ]
}
```
The parent directory inside the container is created if needed, glob patterns are expanded on the host, and ownership is set to `discourse:discourse` so files stay readable by the working user.

### dv data
Print the data directory path (`${XDG_DATA_HOME}/dv`).

```bash
dv data
```

### dv config completion
Generate shell completion scripts (rarely needed). For zsh:

```bash
dv config completion zsh           # print to stdout
dv config completion zsh --install # install to ~/.local/share/zsh/site-functions/_dv
```

### dv upgrade
Download and replace the current binary with the latest GitHub release (or a specific tag).

```bash
dv upgrade           # install the newest release for your platform
dv upgrade --version v0.3.0
```

The command writes the data to the same path as the running executable, so use `sudo dv upgrade` if `dv` lives somewhere like `/usr/local/bin`.

## Environment Variables

Automatically passed through when set on the host:

- `CURSOR_API_KEY`
- `MISTRAL_API_KEY`
- `ANTHROPIC_API_KEY`
- `OPENAI_API_KEY`
- `AWS_ACCESS_KEY_ID`
- `AWS_SECRET_ACCESS_KEY`
- `CLAUDE_CODE_USE_BEDROCK`
- `DEEPSEEK_API_KEY`
- `GEMINI_API_KEY`
- `AMP_API_KEY`
- `GH_TOKEN`
- `OPENROUTER_API_KEY`
- `FACTORY_API_KEY`

### Build acceleration toggles

Set these on the host to change how `dv build` (and other build helpers) behave:

- `DV_DISABLE_BUILDX` — force legacy `docker build` even if buildx is available.
- `DV_BUILDX_BUILDER` (or `DV_BUILDER`) — default builder name used for `docker buildx build`, useful for remote builders.

## Container Details

The image is based on `discourse/discourse_dev:release` and includes:
- Full Discourse development environment at `/var/www/discourse`
- Ruby/Rails stack with bundled dependencies
- Node.js (pnpm) + Ember CLI dev server
- Databases created and migrated for dev/test
- Development tools (vim, ripgrep)
- Helper tools installed for code agents
 - Playwright and system deps preinstalled

## Logs

Runit services log to the following locations inside the container:

| Service    | Log Path                              |
|------------|---------------------------------------|
| unicorn    | `/var/www/discourse/log/unicorn.log`  |
| ember-cli  | `/var/www/discourse/log/ember-cli.log`|
| caddy      | `/var/log/caddy.log`                  |
| postgresql | `/var/log/postgres/current`           |
| redis      | `/var/log/redis/current`              |

View logs with:
```bash
dv run -- tail -f /var/www/discourse/log/unicorn.log
dv run -- tail -f /var/www/discourse/log/ember-cli.log
dv run --root -- tail /var/log/caddy.log
dv run --root -- tail /var/log/postgres/current
dv run --root -- tail /var/log/redis/current
```

## File Structure

```
.
├── internal/
│   └── assets/
│       ├── Dockerfile      # Embedded container definition used by dv build
│       └── dockerfile.go   # Embed/resolve logic (env + XDG overrides)
├── cmd/
│   └── dv/                 # dv binary entrypoint
├── internal/
│   ├── cli/                # dv subcommands (build, run, stop, ...)
│   ├── config/             # JSON config load/save
│   ├── docker/             # Docker CLI wrappers
│   └── xdg/                # XDG path helpers
├── bin/                    # Legacy bash scripts (being replaced by dv)
├── README.md
└── ai-agents.md            # Guidance for AI agents contributing here
```

## Development Workflow (using dv)

1. Build image:
   ```bash
   dv build
   ```
2. Develop inside the container:
   ```bash
   dv start
   dv enter
   # Work with Discourse at /var/www/discourse
   ```
3. Extract changes to a local clone and commit:
   ```bash
   dv extract
   # For the default Discourse workdir; custom workdirs land in $(dv data)/<slug>_src
   cd $(dv data)/discourse_src
   git add . && git commit -m "Your message"
   ```

## Releases

This project uses automated GitHub releases with cross-platform binary builds for macOS and Linux.

### Creating a Release

1. **Using the release script** (recommended):
   ```bash
   ./scripts/release.sh v1.0.0
   # or automatically bump the patch version based on the latest GitHub release
   ./scripts/release.sh --auto
   ```

2. **Manual process**:
   ```bash
   git tag -a v1.0.0 -m "Release v1.0.0"
   git push origin v1.0.0
   ```

### What Happens Automatically

When you push a tag starting with `v`, GitHub Actions will:

1. **Build binaries** for:
   - Linux (amd64, arm64)
   - macOS (amd64, arm64)

2. **Create a GitHub release** with:
   - Release notes from git commits
   - Binary downloads for each platform
   - Checksums for verification

3. **Archive format**:
   - Linux: `.tar.gz`
   - macOS: `.tar.gz`
   - All platforms include README.md and LICENSE

### Version Information

Check the version of your `dv` binary:
```bash
dv version
```

This will show the version, git commit, and build date.

### Release Configuration

The release process is configured in:
- `.github/workflows/release.yml` - GitHub Actions workflow
- `.goreleaser.yml` - GoReleaser configuration for builds and packaging
