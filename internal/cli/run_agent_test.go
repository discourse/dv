package cli

import (
	"testing"

	"dv/internal/config"
)

func TestBuildAgentArgsCodexIncludesSearchBeforeExec(t *testing.T) {
	args := buildAgentArgs("codex", "abc")
	if len(args) < 4 {
		t.Fatalf("unexpected args: %#v", args)
	}
	if args[0] != "codex" {
		t.Fatalf("first arg = %q", args[0])
	}
	// Ensure defaults are injected before subcommand 'exec'
	idxExec := -1
	idxSearch := -1
	for i, a := range args {
		if a == "exec" {
			idxExec = i
		}
		if a == "--search" {
			idxSearch = i
		}
	}
	if idxExec == -1 || idxSearch == -1 || idxSearch > idxExec {
		t.Fatalf("expected --search before exec in %#v", args)
	}
}

func TestBuildAgentArgsCodexPromptAtEnd(t *testing.T) {
	args := buildAgentArgs("codex", "abc")
	if got := args[len(args)-1]; got != "abc" {
		t.Fatalf("last arg = %q, want prompt", got)
	}
}

func TestBuildAgentArgsTermLLMPutsDeveloperAndYoloAfterAsk(t *testing.T) {
	args := buildAgentArgs("term-llm", "hello")
	want := []string{"term-llm", "ask", "@developer", "--yolo", "hello"}
	if len(args) != len(want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %#v, want %#v", args, want)
		}
	}
}

func TestBuildCustomAgentArgs(t *testing.T) {
	cfg := config.Config{Agents: map[string]config.AgentConfig{
		"my-agent": {
			Command:         "custom-cli",
			Args:            []string{"run"},
			PromptArgs:      []string{"--prompt"},
			InteractiveArgs: []string{"--interactive"},
			Aliases:         []string{"ma"},
		},
	}}

	agent := resolveAgentAliasWithConfig(cfg, "ma")
	if agent != "my-agent" {
		t.Fatalf("expected alias to resolve to my-agent, got %q", agent)
	}

	promptArgs := buildAgentArgsWithConfig(cfg, agent, "hello")
	wantPrompt := []string{"custom-cli", "run", "--prompt", "hello"}
	if len(promptArgs) != len(wantPrompt) {
		t.Fatalf("expected %v, got %v", wantPrompt, promptArgs)
	}
	for i := range wantPrompt {
		if promptArgs[i] != wantPrompt[i] {
			t.Fatalf("expected %v, got %v", wantPrompt, promptArgs)
		}
	}

	interactiveArgs := buildAgentInteractiveWithConfig(cfg, agent)
	wantInteractive := []string{"custom-cli", "run", "--interactive"}
	if len(interactiveArgs) != len(wantInteractive) {
		t.Fatalf("expected %v, got %v", wantInteractive, interactiveArgs)
	}
	for i := range wantInteractive {
		if interactiveArgs[i] != wantInteractive[i] {
			t.Fatalf("expected %v, got %v", wantInteractive, interactiveArgs)
		}
	}

	rawArgs := buildAgentRawWithConfig(cfg, agent, []string{"--help"})
	wantRaw := []string{"custom-cli", "run", "--help"}
	if len(rawArgs) != len(wantRaw) {
		t.Fatalf("expected %v, got %v", wantRaw, rawArgs)
	}
	for i := range wantRaw {
		if rawArgs[i] != wantRaw[i] {
			t.Fatalf("expected %v, got %v", wantRaw, rawArgs)
		}
	}
}

func TestCustomAgentOverridesBuiltinAlias(t *testing.T) {
	cfg := config.Config{Agents: map[string]config.AgentConfig{
		"my-agent": {
			Command: "custom-cli",
			Aliases: []string{"tl"},
		},
	}}

	agent := resolveAgentAliasWithConfig(cfg, "tl")
	if agent != "my-agent" {
		t.Fatalf("expected alias to resolve to my-agent, got %q", agent)
	}
	args := buildAgentArgsWithConfig(cfg, agent, "hello")
	want := []string{"custom-cli", "hello"}
	if len(args) != len(want) {
		t.Fatalf("expected %v, got %v", want, args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, args)
		}
	}
}
