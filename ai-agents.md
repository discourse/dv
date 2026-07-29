# AI Agents Guide

This guide helps agents work effectively on the `dv` CLI codebase - a Go tool for managing Discourse development containers.

## Repository Purpose

- Provision Discourse development environments via Docker
- CLI (`dv`) manages container lifecycle and syncs code changes to host Git workflow

## Project Structure

```
main.go                     # CLI entrypoint
internal/
  cli/                      # Cobra subcommands (one file per command)
    root.go                 # Command wiring
    build.go, start.go, ... # Individual commands
  config/                   # JSON config management
  docker/                   # Docker CLI wrappers
  xdg/                      # XDG path helpers
  assets/
    Dockerfile              # Embedded base image
    dockerfile.go           # Dockerfile resolution logic
```

## Development

### Build

```bash
go build -o dv .
```

### Test

```bash
go test ./...
```

### Format & Lint

After editing Go files:

```bash
gofmt -s -w .
goimports -w .
```

Install goimports if needed:

```bash
go install golang.org/x/tools/cmd/goimports@latest
```

## Key Concepts

### XDG Paths

- Config: `${XDG_CONFIG_HOME}/dv/config.json` (default: `~/.config/dv/config.json`)
- Data: `${XDG_DATA_HOME}/dv` (default: `~/.local/share/dv`)
- `selectedAgent` in config identifies the current container

### Dockerfile Resolution (precedence)

1. `DV_DOCKERFILE` env variable
2. `${XDG_CONFIG_HOME}/dv/Dockerfile.local`
3. Embedded default with tracked SHA

## Adding New Commands

1. Create `internal/cli/<command>.go`
2. Wire it in `internal/cli/root.go`
3. Use `internal/config` for persistent settings
4. Use `internal/docker` helpers for container operations

Commands with Cobra positional `Args` validation automatically show their help on stderr and exit non-zero when required arguments are entirely absent. Keep validation declarative (`cobra.ExactArgs`, `cobra.MinimumNArgs`, and similar) when possible so this behavior remains consistent.

Choose container helpers according to command semantics:

- `prepareContainerExecContext` is for execution commands such as `enter` and `run`; it may start the container, run `postStart` hooks, and apply `copyRules`.
- `resolveContainerTarget` is configuration-only and is appropriate for inspection commands; callers must explicitly decide whether a missing or stopped container is an error and must opt into any lifecycle or copy behavior.
- Discourse-specific commands should validate the image kind and use the image's canonical workdir when they need the Rails application root. General workspace commands should use the effective per-container workdir.

## CLI Quick Reference

Core commands for development/testing:

- `dv build` - Build Docker image
- `dv start` - Create/start container
- `dv enter` - Interactive shell in container
- `dv run -- <cmd>` - Run command in container

Run `dv --help` or `dv <command> --help` for full documentation.

## Environment Variables

Auto-passed to container: `CURSOR_API_KEY`, `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, `CLAUDE_CODE_USE_BEDROCK`, `DEEPSEEK_API_KEY`, `GEMINI_API_KEY`, `GH_TOKEN`, `OPENROUTER_API_KEY`, `VENICE_API_KEY`, `FACTORY_API_KEY`, `MISTRAL_API_KEY`, `XAI_API_KEY`, `ANTHROPIC_DEFAULT_SONNET_MODEL`, `ANTHROPIC_DEFAULT_OPUS_MODEL`, `ANTHROPIC_DEFAULT_HAIKU_MODEL`.

Host-side:
- `DV_AGENT` - Override selected container for current process

## Safety Constraints

- Don't modify `internal/assets/Dockerfile` without explicit instructions
- Don't create git commits - the maintainer will review and commit
- Avoid long-lived background processes

## Troubleshooting

- Port conflict: use `--host-port` flag or stop conflicting service
- Missing port mapping: recreate with `dv start --reset`
- Dockerfile confusion: `dv build` shows which path was used
