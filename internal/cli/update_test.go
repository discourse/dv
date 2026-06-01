package cli

import (
	"testing"

	"dv/internal/config"
)

func TestResolveAgentUpdateStepsTargetsSingleAgent(t *testing.T) {
	steps, name, err := resolveAgentUpdateSteps(config.Config{}, "term-llm")
	if err != nil {
		t.Fatalf("resolveAgentUpdateSteps() error = %v", err)
	}
	if name != "term-llm" {
		t.Fatalf("name = %q, want %q", name, "term-llm")
	}
	if len(steps) != 1 {
		t.Fatalf("len(steps) = %d, want 1", len(steps))
	}
	if steps[0].label != "Term-LLM" {
		t.Fatalf("step label = %q, want %q", steps[0].label, "Term-LLM")
	}
}

func TestResolveAgentUpdateStepsSupportsAlias(t *testing.T) {
	steps, name, err := resolveAgentUpdateSteps(config.Config{}, "tl")
	if err != nil {
		t.Fatalf("resolveAgentUpdateSteps() error = %v", err)
	}
	if name != "term-llm" {
		t.Fatalf("name = %q, want %q", name, "term-llm")
	}
	if len(steps) != 1 || steps[0].name != "term-llm" {
		t.Fatalf("steps = %#v, want only term-llm", steps)
	}
}

func TestResolveAgentUpdateStepsEmptyReturnsAllAgents(t *testing.T) {
	steps, name, err := resolveAgentUpdateSteps(config.Config{}, "")
	if err != nil {
		t.Fatalf("resolveAgentUpdateSteps() error = %v", err)
	}
	if name != "" {
		t.Fatalf("name = %q, want empty", name)
	}
	if len(steps) != len(agentUpdateSteps) {
		t.Fatalf("len(steps) = %d, want %d", len(steps), len(agentUpdateSteps))
	}
}

func TestResolveAgentUpdateStepsRejectsUnknownAgent(t *testing.T) {
	if _, _, err := resolveAgentUpdateSteps(config.Config{}, "not-an-agent"); err == nil {
		t.Fatal("resolveAgentUpdateSteps() error = nil, want error")
	}
}

func TestResolveAgentUpdateStepsSupportsCustomAgent(t *testing.T) {
	cfg := config.Config{Agents: map[string]config.AgentConfig{
		"my-agent": {
			Install:       "npm install -g my-agent",
			Update:        "my-agent self-update",
			UpdateAsRoot:  true,
			Aliases:       []string{"ma"},
			InstallAsRoot: false,
		},
	}}

	steps, name, err := resolveAgentUpdateSteps(cfg, "ma")
	if err != nil {
		t.Fatalf("resolveAgentUpdateSteps() error = %v", err)
	}
	if name != "my-agent" {
		t.Fatalf("name = %q, want my-agent", name)
	}
	if len(steps) != 1 {
		t.Fatalf("len(steps) = %d, want 1", len(steps))
	}
	if steps[0].command != "my-agent self-update" {
		t.Fatalf("command = %q, want update command", steps[0].command)
	}
	if !steps[0].runAsRoot {
		t.Fatal("runAsRoot = false, want true")
	}
}

func TestResolveAgentUpdateStepsCustomUsesInstallFallback(t *testing.T) {
	cfg := config.Config{Agents: map[string]config.AgentConfig{
		"my-agent": {
			Install:       "npm install -g my-agent",
			InstallAsRoot: true,
		},
	}}

	steps, _, err := resolveAgentUpdateSteps(cfg, "my-agent")
	if err != nil {
		t.Fatalf("resolveAgentUpdateSteps() error = %v", err)
	}
	if steps[0].command != "npm install -g my-agent" {
		t.Fatalf("command = %q, want install fallback", steps[0].command)
	}
	if !steps[0].runAsRoot {
		t.Fatal("runAsRoot = false, want true")
	}
}

func TestResolveAgentUpdateStepsAllIncludesCustomAgents(t *testing.T) {
	cfg := config.Config{Agents: map[string]config.AgentConfig{
		"my-agent": {
			Update: "my-agent self-update",
		},
	}}

	steps, name, err := resolveAgentUpdateSteps(cfg, "")
	if err != nil {
		t.Fatalf("resolveAgentUpdateSteps() error = %v", err)
	}
	if name != "" {
		t.Fatalf("name = %q, want empty", name)
	}
	if len(steps) != len(agentUpdateSteps)+1 {
		t.Fatalf("len(steps) = %d, want %d", len(steps), len(agentUpdateSteps)+1)
	}
	last := steps[len(steps)-1]
	if last.name != "my-agent" || last.command != "my-agent self-update" {
		t.Fatalf("last step = %#v, want my-agent custom update", last)
	}
}

func TestResolveAgentUpdateStepsCustomOverridesBuiltin(t *testing.T) {
	cfg := config.Config{Agents: map[string]config.AgentConfig{
		"codex": {
			Update: "my-codex self-update",
		},
	}}

	steps, name, err := resolveAgentUpdateSteps(cfg, "codex")
	if err != nil {
		t.Fatalf("resolveAgentUpdateSteps() error = %v", err)
	}
	if name != "codex" {
		t.Fatalf("name = %q, want codex", name)
	}
	if len(steps) != 1 || steps[0].command != "my-codex self-update" {
		t.Fatalf("steps = %#v, want custom codex update", steps)
	}

	allSteps, _, err := resolveAgentUpdateSteps(cfg, "")
	if err != nil {
		t.Fatalf("resolveAgentUpdateSteps(all) error = %v", err)
	}
	codexCount := 0
	for _, step := range allSteps {
		if step.name == "codex" {
			codexCount++
			if step.command != "my-codex self-update" {
				t.Fatalf("codex command = %q, want custom update", step.command)
			}
		}
	}
	if codexCount != 1 {
		t.Fatalf("codexCount = %d, want 1", codexCount)
	}
}

func TestResolveAgentUpdateStepsCustomAliasOverridesBuiltinAlias(t *testing.T) {
	cfg := config.Config{Agents: map[string]config.AgentConfig{
		"my-agent": {
			Aliases: []string{"tl"},
			Update:  "my-agent self-update",
		},
	}}

	steps, name, err := resolveAgentUpdateSteps(cfg, "tl")
	if err != nil {
		t.Fatalf("resolveAgentUpdateSteps() error = %v", err)
	}
	if name != "my-agent" {
		t.Fatalf("name = %q, want my-agent", name)
	}
	if len(steps) != 1 || steps[0].command != "my-agent self-update" {
		t.Fatalf("steps = %#v, want custom alias update", steps)
	}
}
